package helm

import (
	"context"
	"fmt"
	"github.com/Masterminds/semver/v3"
	"github.com/kluctl/kluctl/v2/pkg/registries"
	"github.com/kluctl/kluctl/v2/pkg/status"
	"github.com/kluctl/kluctl/v2/pkg/utils"
	"github.com/kluctl/kluctl/v2/pkg/utils/uo"
	"github.com/kluctl/kluctl/v2/pkg/yaml"
	cp "github.com/otiai10/copy"
	"github.com/rogpeppe/go-internal/lockedfile"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type HelmCredentialsProvider interface {
	FindCredentials(repoUrl string, credentialsId *string) *repo.Entry
}

type Chart struct {
	repo      string
	localPath string
	chartName string

	credentials   HelmCredentialsProvider
	credentialsId string

	versions []string
}

func NewChart(repo string, localPath string, chartName string, credentialsProvider HelmCredentialsProvider, credentialsId string) (*Chart, error) {
	hc := &Chart{
		repo:          repo,
		localPath:     localPath,
		credentials:   credentialsProvider,
		credentialsId: credentialsId,
	}

	if localPath == "" && repo == "" {
		return nil, fmt.Errorf("repo and localPath are missing")
	}

	if hc.IsLocalChart() {
		chartYaml, err := uo.FromFile(yaml.FixPathExt(filepath.Join(hc.localPath, "Chart.yaml")))
		if err != nil {
			return nil, err
		}
		x, _, err := chartYaml.GetNestedString("name")
		if err != nil {
			return nil, err
		}
		if x == "" {
			return nil, fmt.Errorf("invalid/empty chart name")
		}
		hc.chartName = x
	} else if registry.IsOCI(repo) {
		if chartName != "" {
			return nil, fmt.Errorf("chartName can't be specified when using OCI repos")
		}
		s := strings.Split(repo, "/")
		chartName := s[len(s)-1]
		if m, _ := regexp.MatchString(`[a-zA-Z_-]+`, chartName); !m {
			return nil, fmt.Errorf("invalid oci chart url: %s", repo)
		}
		hc.chartName = chartName
	} else if chartName == "" {
		return nil, fmt.Errorf("chartName is missing")
	} else {
		hc.chartName = chartName
	}

	return hc, nil
}

func (c *Chart) GetRepo() string {
	return c.repo
}

func (c *Chart) GetLocalPath() string {
	return c.localPath
}

func (c *Chart) IsLocalChart() bool {
	return c.localPath != ""
}

func (c *Chart) GetLocalChartVersion() (string, error) {
	chartYaml, err := uo.FromFile(yaml.FixPathExt(filepath.Join(c.localPath, "Chart.yaml")))
	if err != nil {
		return "", err
	}
	x, _, err := chartYaml.GetNestedString("version")
	if err != nil {
		return "", err
	}
	if x == "" {
		return "", fmt.Errorf("invalid/empty chart version")
	}
	return x, nil
}

func (c *Chart) BuildPulledChartDir(baseDir string, version string) (string, error) {
	u, err := url.Parse(c.repo)
	if err != nil {
		return "", err
	}

	scheme := ""
	port := ""
	switch {
	case u.Scheme == "oci":
		scheme = "oci"
	case u.Scheme == "http":
		scheme = "http"
		if u.Port() != "80" {
			port = u.Port()
		}
	case u.Scheme == "https":
		scheme = "https"
		if u.Port() != "443" {
			port = u.Port()
		}
	default:
		return "", fmt.Errorf("unsupported scheme in %s", u.String())
	}
	if port != "" {
		scheme += "_" + port
	}

	dir := filepath.Join(
		baseDir,
		fmt.Sprintf("%s_%s", scheme, strings.ToLower(u.Hostname())),
		filepath.FromSlash(strings.ToLower(u.Path)),
	)
	if u.Scheme != "oci" {
		dir = filepath.Join(dir, c.chartName)
	}
	if version != "" {
		dir = filepath.Join(dir, version)
	}
	err = utils.CheckInDir(baseDir, dir)
	if err != nil {
		return "", err
	}

	return dir, nil
}

func (c *Chart) GetChartName() string {
	return c.chartName
}

func (c *Chart) PullToTmp(ctx context.Context, version string) (*PulledChart, error) {
	if c.IsLocalChart() {
		return nil, fmt.Errorf("can not pull local charts")
	}

	tmpPullDir, err := os.MkdirTemp(utils.GetTmpBaseDir(ctx), c.chartName+"-pull-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpPullDir)

	cfg, err := buildHelmConfig(nil)
	if err != nil {
		return nil, err
	}
	a := action.NewPullWithOpts(action.WithConfig(cfg))
	a.Settings = cli.New()
	a.Untar = true
	a.DestDir = tmpPullDir
	a.Version = version

	if c.credentialsId != "" {
		if registry.IsOCI(c.repo) {
			return nil, fmt.Errorf("OCI charts can currently only be authenticated via registry login and not via cli arguments")
		}
		if c.credentials == nil {
			return nil, fmt.Errorf("no credentials provider")
		}
	}

	if c.credentials != nil {
		p := &c.credentialsId
		if c.credentialsId == "" {
			p = nil
		}
		creds := c.credentials.FindCredentials(c.repo, p)
		if creds != nil {
			a.Username = creds.Username
			a.Password = creds.Password
			a.CertFile = creds.CertFile
			a.CaFile = creds.CAFile
			a.KeyFile = creds.KeyFile
			a.InsecureSkipTLSverify = creds.InsecureSkipTLSverify
			a.PassCredentialsAll = creds.PassCredentialsAll
		}
	}

	var out string
	if registry.IsOCI(c.repo) {
		out, err = a.Run(c.repo)
	} else {
		a.RepoURL = c.repo
		out, err = a.Run(c.chartName)
	}
	if out != "" {
		status.PlainText(ctx, out)
	}
	if err != nil {
		return nil, err
	}

	chartDir, err := os.MkdirTemp(utils.GetTmpBaseDir(ctx), c.chartName+"-pulled-")
	if err != nil {
		return nil, err
	}

	// move chart
	des, err := os.ReadDir(filepath.Join(tmpPullDir, c.chartName))
	if err != nil {
		return nil, err
	}
	for _, de := range des {
		err = os.Rename(filepath.Join(tmpPullDir, c.chartName, de.Name()), filepath.Join(chartDir, de.Name()))
		if err != nil {
			return nil, err
		}
	}

	return NewPulledChart(c, version, chartDir, true), nil
}

func (c *Chart) Pull(ctx context.Context, pc *PulledChart) error {
	if c.IsLocalChart() {
		return fmt.Errorf("can not pull local charts")
	}

	newPulled, err := c.PullToTmp(ctx, pc.version)
	if err != nil {
		return err
	}
	defer os.RemoveAll(newPulled.dir)

	err = os.RemoveAll(pc.dir)
	if err != nil {
		return err
	}

	_ = os.MkdirAll(filepath.Dir(pc.dir), 0o755)

	err = cp.Copy(newPulled.dir, pc.dir)
	if err != nil {
		return err
	}

	return nil
}

func (c *Chart) doPullCached(ctx context.Context, version string) (*PulledChart, *lockedfile.File, error) {
	baseDir := filepath.Join(utils.GetTmpBaseDir(ctx), "helm-charts")
	cacheDir, err := c.BuildPulledChartDir(baseDir, version)
	_ = os.MkdirAll(cacheDir, 0o755)

	lock, err := lockedfile.Create(cacheDir + ".lock")
	if err != nil {
		return nil, nil, err
	}

	cached := NewPulledChart(c, version, cacheDir, true)
	needsPull, _, _, err := cached.CheckNeedsPull()
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	if !needsPull {
		return cached, lock, nil
	}

	err = c.Pull(ctx, cached)
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}

	return cached, lock, nil
}

func (c *Chart) PullCached(ctx context.Context, version string) (*PulledChart, error) {
	if c.IsLocalChart() {
		return nil, fmt.Errorf("can not pull local charts")
	}

	pc, lock, err := c.doPullCached(ctx, version)
	if err != nil {
		return nil, err
	}
	_ = lock.Close()
	return pc, nil
}

func (c *Chart) PullInProject(ctx context.Context, baseDir string, version string) (*PulledChart, error) {
	if c.IsLocalChart() {
		return nil, fmt.Errorf("can not pull local charts")
	}

	cachePc, lock, err := c.doPullCached(ctx, version)
	if err != nil {
		return nil, err
	}
	defer lock.Close()

	pc, err := c.GetPulledChart(baseDir, version)
	if err != nil {
		return nil, err
	}

	err = os.RemoveAll(pc.dir)
	if err != nil {
		return nil, err
	}

	err = cp.Copy(cachePc.dir, pc.dir)
	if err != nil {
		return nil, err
	}

	return pc, nil
}

func (c *Chart) GetPulledChart(baseDir string, version string) (*PulledChart, error) {
	if c.IsLocalChart() {
		return nil, fmt.Errorf("can not pull local charts")
	}

	chartDir, err := c.BuildPulledChartDir(baseDir, version)
	if err != nil {
		return nil, err
	}
	return NewPulledChart(c, version, chartDir, false), nil
}

func (c *Chart) QueryVersions(ctx context.Context) error {
	if c.IsLocalChart() {
		return fmt.Errorf("can not query versions for local charts")
	}

	if registry.IsOCI(c.repo) {
		return c.queryVersionsOci(ctx)
	}
	return c.queryVersionsHelmRepo(ctx)
}

func (c *Chart) queryVersionsOci(ctx context.Context) error {
	rh := registries.NewRegistryHelper(ctx)

	imageName := strings.TrimPrefix(c.repo, "oci://")
	tags, err := rh.ListImageTags(imageName)
	if err != nil {
		return err
	}

	c.versions = tags

	return nil
}

func (c *Chart) queryVersionsHelmRepo(ctx context.Context) error {
	settings := cli.New()

	var e *repo.Entry
	if c.credentialsId != "" {
		if c.credentials == nil {
			return fmt.Errorf("no credentials provider")
		}
		e = c.credentials.FindCredentials(c.repo, &c.credentialsId)
		if e == nil {
			return fmt.Errorf("no credentials provided for Chart %s", c.chartName)
		}
	} else {
		e = &repo.Entry{
			URL: c.repo,
		}
	}

	r, err := repo.NewChartRepository(e, getter.All(settings))
	if err != nil {
		return err
	}

	r.CachePath, err = os.MkdirTemp(utils.GetTmpBaseDir(ctx), "helm-check-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(r.CachePath)

	indexFile, err := r.DownloadIndexFile()
	if err != nil {
		return err
	}

	index, err := repo.LoadIndexFile(indexFile)
	if err != nil {
		return err
	}

	indexEntry, ok := index.Entries[c.chartName]
	if !ok || len(indexEntry) == 0 {
		return fmt.Errorf("helm chart %s not found in Repo index", c.chartName)
	}

	c.versions = make([]string, 0, indexEntry.Len())
	for _, x := range indexEntry {
		c.versions = append(c.versions, x.Version)
	}

	return nil
}

func (c *Chart) GetLatestVersion(constraints *string) (string, error) {
	if len(c.versions) == 0 {
		return "", fmt.Errorf("no versions found or queried")
	}

	var err error
	var updateConstraints *semver.Constraints
	if constraints != nil {
		updateConstraints, err = semver.NewConstraint(*constraints)
		if err != nil {
			return "", fmt.Errorf("invalid constraints '%s': %w", *constraints, err)
		}
	}

	var versions semver.Collection
	for _, x := range c.versions {
		v, err := semver.NewVersion(x)
		if err != nil {
			continue
		}

		if updateConstraints == nil {
			if v.Prerelease() != "" {
				// we don't allow pre-releases by default. To allow pre-releases, use 1.0.0-0 as constraint
				continue
			}
		} else if !updateConstraints.Check(v) {
			continue
		}
		versions = append(versions, v)
	}
	if len(versions) == 0 {
		if constraints == nil {
			return "", fmt.Errorf("no version found")
		} else {
			return "", fmt.Errorf("no version found that satisfies constraints '%s'", *constraints)
		}
	}

	sort.Stable(versions)
	latestVersion := versions[len(versions)-1].Original()
	return latestVersion, nil
}