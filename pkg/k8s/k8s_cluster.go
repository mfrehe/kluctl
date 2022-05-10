package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	goversion "github.com/hashicorp/go-version"
	"github.com/kluctl/kluctl/v2/pkg/status"
	"github.com/kluctl/kluctl/v2/pkg/types/k8s"
	"github.com/kluctl/kluctl/v2/pkg/utils"
	"github.com/kluctl/kluctl/v2/pkg/utils/uo"
	"github.com/kluctl/kluctl/v2/pkg/yaml"
	"io"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"net/http"
	"sync"
	"time"
)

var (
	deprecatedResources = map[schema.GroupKind]bool{
		{Group: "extensions", Kind: "Ingress"}: true,
	}
)

type K8sCluster struct {
	ctx context.Context

	DryRun bool

	restConfig *rest.Config
	clientPool chan *parallelClientEntry

	ServerVersion *goversion.Version

	Resources *k8sResources
}

type parallelClientEntry struct {
	http           *http.Client
	corev1         *corev1.CoreV1Client
	dynamicClient  dynamic.Interface
	metadataClient metadata.Interface

	warnings []ApiWarning
}

type ApiWarning struct {
	Code  int
	Agent string
	Text  string
}

func (p *parallelClientEntry) HandleWarningHeader(code int, agent string, text string) {
	p.warnings = append(p.warnings, ApiWarning{
		Code:  code,
		Agent: agent,
		Text:  text,
	})
}

const parallelClients = 16

func NewK8sCluster(ctx context.Context, configIn *rest.Config, dryRun bool) (*K8sCluster, error) {
	restConfig := rest.CopyConfig(configIn)
	restConfig.QPS = 10
	restConfig.Burst = 20

	resources, err := newK8sResources(ctx, restConfig)
	if err != nil {
		return nil, err
	}

	k := &K8sCluster{
		ctx:        ctx,
		DryRun:     dryRun,
		restConfig: restConfig,
		Resources:  resources,
	}

	err = k.initClientPool()
	if err != nil {
		return nil, err
	}

	v, err := k.Resources.discovery.ServerVersion()
	if err != nil {
		return nil, err
	}
	v2, err := goversion.NewVersion(v.String())
	if err != nil {
		return nil, err
	}
	k.ServerVersion = v2

	err = k.Resources.updateResources(true)
	if err != nil {
		return nil, err
	}

	return k, nil
}

func (k *K8sCluster) initClientPool() error {
	var err error

	if k.clientPool != nil {
		for i := 0; i < parallelClients; i++ {
			p := <-k.clientPool
			p.http.CloseIdleConnections()
		}
	}

	k.clientPool = make(chan *parallelClientEntry, parallelClients)
	for i := 0; i < parallelClients; i++ {
		p := &parallelClientEntry{}
		config := rest.CopyConfig(k.restConfig)
		config.WarningHandler = p

		p.http, err = rest.HTTPClientFor(config)
		if err != nil {
			return err
		}

		p.corev1, err = corev1.NewForConfigAndClient(config, p.http)
		if err != nil {
			return err
		}

		p.dynamicClient, err = dynamic.NewForConfigAndClient(config, p.http)
		if err != nil {
			return err
		}

		p.metadataClient, err = metadata.NewForConfigAndClient(config, p.http)
		if err != nil {
			return err
		}

		k.clientPool <- p
	}
	return nil
}

func (k *K8sCluster) ReadWrite() *K8sCluster {
	k2 := *k
	k2.DryRun = false
	return &k2
}

func (k *K8sCluster) GetCA() []byte {
	return k.restConfig.CAData
}

func (k *K8sCluster) withClientFromPool(cb func(p *parallelClientEntry) error) ([]ApiWarning, error) {
	select {
	case p := <-k.clientPool:
		defer func() { k.clientPool <- p }()
		p.warnings = nil
		err := cb(p)
		return append([]ApiWarning(nil), p.warnings...), err
	case <-k.ctx.Done():
		return nil, fmt.Errorf("failed waiting for free client: %w", k.ctx.Err())
	}
}

func (k *K8sCluster) withDynamicClientForGVK(gvk schema.GroupVersionKind, namespace string, cb func(r dynamic.ResourceInterface) error) ([]ApiWarning, error) {
	return k.withClientFromPool(func(p *parallelClientEntry) error {
		gvr, namespaced, err := k.Resources.GetGVRForGVK(gvk)
		if err != nil {
			return err
		}

		if namespaced && namespace != "" {
			return cb(p.dynamicClient.Resource(*gvr).Namespace(namespace))
		} else {
			return cb(p.dynamicClient.Resource(*gvr))
		}
	})
}

func (k *K8sCluster) withMetadataClientForGVK(gvk schema.GroupVersionKind, namespace string, cb func(r metadata.ResourceInterface) error) ([]ApiWarning, error) {
	return k.withClientFromPool(func(p *parallelClientEntry) error {
		gvr, namespaced, err := k.Resources.GetGVRForGVK(gvk)
		if err != nil {
			return err
		}

		if namespaced && namespace != "" {
			return cb(p.metadataClient.Resource(*gvr).Namespace(namespace))
		} else {
			return cb(p.metadataClient.Resource(*gvr))
		}
	})
}

func (k *K8sCluster) buildLabelSelector(labels map[string]string) string {
	ret := ""

	for k, v := range labels {
		if len(ret) != 0 {
			ret += ","
		}
		ret += fmt.Sprintf("%s=%s", k, v)
	}
	return ret
}

func (k *K8sCluster) ListObjects(gvk schema.GroupVersionKind, namespace string, labels map[string]string) ([]*uo.UnstructuredObject, []ApiWarning, error) {
	var result []*uo.UnstructuredObject

	apiWarnings, err := k.withDynamicClientForGVK(gvk, namespace, func(r dynamic.ResourceInterface) error {
		o := v1.ListOptions{
			LabelSelector: k.buildLabelSelector(labels),
		}
		x, err := r.List(k.ctx, o)
		if err != nil {
			return err
		}
		for _, o := range x.Items {
			result = append(result, uo.FromUnstructured(&o))
		}
		return nil
	})
	return result, apiWarnings, err
}

func (k *K8sCluster) ListObjectsMetadata(gvk schema.GroupVersionKind, namespace string, labels map[string]string) ([]*uo.UnstructuredObject, []ApiWarning, error) {
	var result []*uo.UnstructuredObject

	apiWarnings, err := k.withMetadataClientForGVK(gvk, namespace, func(r metadata.ResourceInterface) error {
		o := v1.ListOptions{
			LabelSelector: k.buildLabelSelector(labels),
		}
		x, err := r.List(k.ctx, o)
		if err != nil {
			return err
		}
		for _, o := range x.Items {
			b, err := json.Marshal(o)
			if err != nil {
				panic(err)
			}
			u, err := uo.FromString(string(b))
			if err != nil {
				panic(err)
			}
			u.SetK8sGVK(gvk)
			result = append(result, u)
		}
		return nil
	})
	return result, apiWarnings, err
}

func (k *K8sCluster) ListAllObjects(verbs []string, namespace string, labels map[string]string, onlyMetadata bool) ([]*uo.UnstructuredObject, map[schema.GroupVersionKind][]ApiWarning, error) {
	wp := utils.NewWorkerPoolWithErrors(8)
	defer wp.StopWait(false)

	var ret []*uo.UnstructuredObject
	retApiWarnings := make(map[schema.GroupVersionKind][]ApiWarning)
	var mutex sync.Mutex

	filter := func(ar *v1.APIResource) bool {
		foundVerb := false
		for _, v := range verbs {
			if utils.FindStrInSlice(ar.Verbs, v) != -1 {
				foundVerb = true
				break
			}
		}
		return foundVerb
	}

	for _, gvk := range k.Resources.GetFilteredPreferredGVKs(filter) {
		wp.Submit(func() error {
			var l []*uo.UnstructuredObject
			var apiWarnings []ApiWarning
			var err error
			if onlyMetadata {
				l, apiWarnings, err = k.ListObjectsMetadata(gvk, namespace, labels)
			} else {
				l, apiWarnings, err = k.ListObjects(gvk, namespace, labels)
			}
			if err != nil && !errors.IsNotFound(err) {
				return err
			}
			mutex.Lock()
			defer mutex.Unlock()
			ret = append(ret, l...)
			if len(apiWarnings) != 0 {
				retApiWarnings[gvk] = apiWarnings
			}
			return nil
		})
	}

	err := wp.StopWait(false)
	if err != nil {
		return nil, retApiWarnings, err
	}
	return ret, retApiWarnings, nil
}

func (k *K8sCluster) GetSingleObject(ref k8s.ObjectRef) (*uo.UnstructuredObject, []ApiWarning, error) {
	var result *uo.UnstructuredObject
	apiWarnings, err := k.withDynamicClientForGVK(ref.GVK, ref.Namespace, func(r dynamic.ResourceInterface) error {
		o := v1.GetOptions{}
		x, err := r.Get(k.ctx, ref.Name, o)
		if err != nil {
			return err
		}
		result = uo.FromUnstructured(x)
		return nil
	})
	return result, apiWarnings, err
}

func (k *K8sCluster) GetObjectsByRefs(refs []k8s.ObjectRef) ([]*uo.UnstructuredObject, map[k8s.ObjectRef][]ApiWarning, error) {
	wp := utils.NewWorkerPoolWithErrors(32)
	defer wp.StopWait(false)

	var ret []*uo.UnstructuredObject
	retApiWarnings := make(map[k8s.ObjectRef][]ApiWarning)
	var mutex sync.Mutex

	for _, ref_ := range refs {
		ref := ref_
		wp.Submit(func() error {
			o, apiWarnings, err := k.GetSingleObject(ref)
			mutex.Lock()
			defer mutex.Unlock()
			if len(apiWarnings) != 0 {
				retApiWarnings[ref] = apiWarnings
			}
			if err != nil {
				if errors.IsNotFound(err) || meta.IsNoMatchError(err) {
					return nil
				}
				return err
			}
			ret = append(ret, o)
			return nil
		})
	}
	err := wp.StopWait(false)
	if err != nil {
		return nil, retApiWarnings, err
	}

	return ret, retApiWarnings, nil
}

type DeleteOptions struct {
	ForceDryRun         bool
	NoWait              bool
	IgnoreNotFoundError bool
}

func (k *K8sCluster) DeleteSingleObject(ref k8s.ObjectRef, options DeleteOptions) ([]ApiWarning, error) {
	dryRun := k.DryRun || options.ForceDryRun

	pp := v1.DeletePropagationForeground
	o := v1.DeleteOptions{
		PropagationPolicy: &pp,
	}
	if dryRun {
		o.DryRun = []string{"All"}
	}

	apiWarnings, err := k.withDynamicClientForGVK(ref.GVK, ref.Namespace, func(r dynamic.ResourceInterface) error {
		err := r.Delete(k.ctx, ref.Name, o)
		if err != nil {
			if options.IgnoreNotFoundError && errors.IsNotFound(err) {
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil {
		return apiWarnings, err
	}

	if !dryRun && !options.NoWait {
		err = k.waitForDeletedObject(ref)
		if err != nil {
			return apiWarnings, err
		}
	}
	return apiWarnings, nil
}

func (k *K8sCluster) waitForDeletedObject(ref k8s.ObjectRef) error {
	for true {
		_, _, err := k.GetSingleObject(ref)

		if err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return err
		}

		select {
		case <-time.After(500 * time.Millisecond):
			continue
		case <-k.ctx.Done():
			return fmt.Errorf("failed waiting for deletion of %s: %w", ref.String(), k.ctx.Err())
		}
	}
	return nil
}

var v1_21, _ = goversion.NewVersion("1.21")
var v1_1000, _ = goversion.NewVersion("1.1000")

func (k *K8sCluster) FixObjectForPatch(o *uo.UnstructuredObject) *uo.UnstructuredObject {
	// A bug in versions < 1.20 cause errors when applying resources that have some fields omitted which have
	// default values. We need to fix these resources.
	// UPDATE even though https://github.com/kubernetes-sigs/structured-merge-diff/issues/130 says it's fixed, the
	// issue is still present.
	needsDefaultsFix := k.ServerVersion.LessThan(v1_21) || true
	// TODO check when this is actually fixed (see https://github.com/kubernetes/kubernetes/issues/94275)
	needsTypeConversionFix := k.ServerVersion.LessThan(v1_1000)
	if !needsDefaultsFix && !needsTypeConversionFix {
		return o
	}

	o = o.Clone()

	fixPorts := func(p string) {
		if !needsDefaultsFix {
			return
		}

		ports, found, _ := uo.NewMyJsonPathMust(p).GetFirstListOfMaps(o)
		if !found {
			return
		}

		for _, port := range ports {
			if _, ok := port["protocol"]; !ok {
				port["protocol"] = "TCP"
			}
		}
	}

	fixStringType := func(p string, k string) {
		if !needsTypeConversionFix {
			return
		}
		d, found, _ := uo.NewMyJsonPathMust(p).GetFirstMap(o)
		if !found {
			return
		}
		v, ok := d[k]
		if !ok {
			return
		}
		_, ok = v.(string)
		if !ok {
			d[k] = fmt.Sprintf("%v", v)
		}
	}

	fixContainer := func(p string) {
		fixPorts(p + ".ports")
		fixStringType(p+"resources.limits", "cpu")
		fixStringType(p+"resources.requests", "cpu")
	}

	fixContainers := func(p string) {
		containers, found, _ := uo.NewMyJsonPathMust(p).GetFirstListOfMaps(o)
		if !found {
			return
		}
		for i, _ := range containers {
			fixContainer(fmt.Sprintf("%s[%d]", p, i))
		}
	}

	fixLimits := func(p string) {
		limits, found, _ := uo.NewMyJsonPathMust(p).GetFirstListOfMaps(o)
		if !found {
			return
		}
		for i, _ := range limits {
			fixStringType(fmt.Sprintf("%s[%d].default", p, i), "cpu")
			fixStringType(fmt.Sprintf("%s[%d].defaultRequest", p, i), "cpu")
		}
	}

	fixContainers("spec.template.spec.containers")
	fixPorts("spec.ports")
	fixLimits("spec.limits")

	return o
}

type PatchOptions struct {
	ForceDryRun bool
	ForceApply  bool
}

func (k *K8sCluster) PatchObject(o *uo.UnstructuredObject, options PatchOptions) (*uo.UnstructuredObject, []ApiWarning, error) {
	dryRun := k.DryRun || options.ForceDryRun
	ref := o.GetK8sRef()

	data, err := yaml.WriteYamlBytes(o)
	if err != nil {
		return nil, nil, err
	}

	po := v1.PatchOptions{
		FieldManager: "kluctl",
	}
	if dryRun {
		po.DryRun = []string{"All"}
	}
	if options.ForceApply {
		po.Force = &options.ForceApply
	}

	status.Trace(k.ctx, "patching %s", ref.String())

	var result *uo.UnstructuredObject
	apiWarnings, err := k.withDynamicClientForGVK(ref.GVK, ref.Namespace, func(r dynamic.ResourceInterface) error {
		x, err := r.Patch(k.ctx, ref.Name, types.ApplyPatchType, data, po)
		if err != nil {
			return fmt.Errorf("failed to patch %s: %w", ref.String(), err)
		}
		result = uo.FromUnstructured(x)
		return nil
	})
	return result, apiWarnings, err
}

type UpdateOptions struct {
	ForceDryRun bool
}

func (k *K8sCluster) UpdateObject(o *uo.UnstructuredObject, options UpdateOptions) (*uo.UnstructuredObject, []ApiWarning, error) {
	dryRun := k.DryRun || options.ForceDryRun
	ref := o.GetK8sRef()

	updateOpts := v1.UpdateOptions{
		FieldManager: "kluctl",
	}
	if dryRun {
		updateOpts.DryRun = []string{"All"}
	}

	status.Trace(k.ctx, "updating %s", ref.String())

	var result *uo.UnstructuredObject
	apiWarnings, err := k.withDynamicClientForGVK(ref.GVK, ref.Namespace, func(r dynamic.ResourceInterface) error {
		x, err := r.Update(k.ctx, o.ToUnstructured(), updateOpts)
		if err != nil {
			return err
		}
		result = uo.FromUnstructured(x)
		return nil
	})
	return result, apiWarnings, err
}

func (k *K8sCluster) ProxyGet(scheme, namespace, name, port, path string, params map[string]string) (io.ReadCloser, error) {
	var ret rest.ResponseWrapper
	_, err := k.withClientFromPool(func(p *parallelClientEntry) error {
		ret = p.corev1.Services(namespace).ProxyGet(scheme, name, port, path, params)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ret.Stream(k.ctx)
}
