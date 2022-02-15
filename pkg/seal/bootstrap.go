package seal

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"github.com/bitnami-labs/sealed-secrets/pkg/crypto"
	"github.com/codablock/kluctl/pkg/k8s"
	"github.com/codablock/kluctl/pkg/types"
	"github.com/codablock/kluctl/pkg/utils/uo"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	certUtil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
	"time"
)

const sealedSecretsKeyLabel = "sealedsecrets.bitnami.com/sealed-secrets-key"
const secretName = "sealed-secrets-key-kluctl-bootstrap"
const configMapName = "sealed-secrets-key-kluctl-bootstrap"

func BootstrapSealedSecrets(k *k8s.K8sCluster, namespace string) error {
	existing, _, err := k.GetSingleObject(types.ObjectRef{
		GVK:  schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
		Name: "sealedsecrets.bitnami.com",
	})
	if existing != nil {
		// no bootstrap needed as the sealed-secrets operator seams to be installed already
		return nil
	}

	existing, _, err = k.GetSingleObject(types.ObjectRef{
		GVK:       schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"},
		Name:      configMapName,
		Namespace: namespace,
	})
	if existing != nil {
		// bootstrap has already been done
		return nil
	}

	log.Infof("Bootstrapping sealed-secrets with a self-generated key")

	key, cert, err := crypto.GeneratePrivateKeyAndCert(2048, 10*365*24*time.Hour, "bootstrap.kluctl.io")
	if err != nil {
		return err
	}

	certs := []*x509.Certificate{cert}
	err = writeKey(k, key, certs, namespace)
	if err != nil {
		return err
	}
	return nil
}

func writeKey(k *k8s.K8sCluster, key *rsa.PrivateKey, certs []*x509.Certificate, namespace string) error {
	certbytes := []byte{}
	for _, cert := range certs {
		certbytes = append(certbytes, pem.EncodeToMemory(&pem.Block{Type: certUtil.CertificateBlockType, Bytes: cert.Raw})...)
	}
	keybytes := pem.EncodeToMemory(&pem.Block{Type: keyutil.RSAPrivateKeyBlockType, Bytes: x509.MarshalPKCS1PrivateKey(key)})

	secret := uo.New()
	secret.SetK8sGVK(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"})
	secret.SetK8sName(secretName)
	secret.SetK8sNamespace(namespace)
	secret.SetK8sLabel(sealedSecretsKeyLabel, "active")
	secret.Object["data"] = map[string]string{
		v1.TLSPrivateKeyKey: base64.StdEncoding.EncodeToString(keybytes),
		v1.TLSCertKey:       base64.StdEncoding.EncodeToString(certbytes),
	}
	secret.Object["type"] = v1.SecretTypeTLS

	configMap := uo.New()
	configMap.SetK8sGVK(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"})
	configMap.SetK8sName(configMapName)
	configMap.SetK8sNamespace(namespace)
	configMap.Object["data"] = map[string]string{
		v1.TLSCertKey: string(certbytes),
	}

	_, _, err := k.PatchObject(secret.ToUnstructured(), k8s.PatchOptions{})
	if err != nil {
		return err
	}
	_, _, err = k.PatchObject(configMap.ToUnstructured(), k8s.PatchOptions{})
	if err != nil {
		return err
	}

	return nil
}