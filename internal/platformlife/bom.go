// Package platformlife is the lifecycle of the managed-services platform (docs/adr/0009-managed-services-platform-lifecycle.md).
// This file reads its bill of materials, deploy/platform/platform-bom.yaml: the exact version and digest of every
// component, shipped inside each release.
package platformlife

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/mutex-solutions/proxcloud/deploy"
)

// BOM is the platform bill of materials. The YAML keys and the JSON keys of GET /admin/platform/bom are the same.
type BOM struct {
	SchemaVersion int          `yaml:"schemaVersion" json:"schemaVersion"`
	Release       string       `yaml:"release" json:"release"` // "-dev" on main, the release version at its tag
	Kubernetes    Kubernetes   `yaml:"kubernetes" json:"kubernetes"`
	Seed          Seed         `yaml:"seed" json:"seed"`
	Images        Images       `yaml:"images" json:"images"`
	CAPI          CAPI         `yaml:"capi" json:"capi"`
	TenantCharts  TenantCharts `yaml:"tenantCharts" json:"tenantCharts"`
	Components    []Component  `yaml:"components" json:"components"`
}

// Kubernetes is the workload cluster's version and the published node image it boots.
type Kubernetes struct {
	Version   string  `yaml:"version" json:"version"`
	NodeImage VMImage `yaml:"nodeImage" json:"nodeImage"`
}

// VMImage is a published VM image (docs/vm-artifact-publishing.md): ghcr.io/mutex-solutions/<package>@<digest>.
type VMImage struct {
	Package string `yaml:"package" json:"package"`
	Tag     string `yaml:"tag" json:"tag"`
	Digest  string `yaml:"digest" json:"digest"`
}

// Seed is the k3s management cluster: the image it is cloned from and the k3s it installs.
type Seed struct {
	Image VMImage `yaml:"image" json:"image"`
	K3s   K3s     `yaml:"k3s" json:"k3s"`
}

// K3s is the k3s release the seed installs, both downloads checked by sha256.
type K3s struct {
	Version       string   `yaml:"version" json:"version"`
	Binary        Download `yaml:"binary" json:"binary"`
	InstallScript Download `yaml:"installScript" json:"installScript"`
}

// Download is a file fetched by URL and checked against its sha256 (64 hex digits).
type Download struct {
	URL    string `yaml:"url" json:"url"`
	SHA256 string `yaml:"sha256" json:"sha256"`
}

// Images are container images referenced outside a chart, as <repo>:<tag>@<digest>.
type Images struct {
	KubeVip    string `yaml:"kubeVip" json:"kubeVip"`
	Clusterctl string `yaml:"clusterctl" json:"clusterctl"`
}

// CAPI is the Cluster API providers of each cluster.
type CAPI struct {
	Seed     Providers `yaml:"seed" json:"seed"`
	Workload Providers `yaml:"workload" json:"workload"`
}

// Providers are clusterctl provider references, <name>:<version>. IPAM and Addon are optional.
type Providers struct {
	Core           string `yaml:"core" json:"core"`
	Bootstrap      string `yaml:"bootstrap" json:"bootstrap"`
	ControlPlane   string `yaml:"controlPlane" json:"controlPlane"`
	Infrastructure string `yaml:"infrastructure" json:"infrastructure"`
	IPAM           string `yaml:"ipam,omitempty" json:"ipam,omitempty"`
	Addon          string `yaml:"addon,omitempty" json:"addon,omitempty"`
}

// TenantCharts are the charts the providers pin for new tenant clusters and databases.
type TenantCharts struct {
	Kubernetes Chart `yaml:"kubernetes" json:"kubernetes"`
	Database   Chart `yaml:"database" json:"database"`
}

// Chart is a Helm chart. Repo is a bare OCI repository (ghcr.io/org/charts) or an https:// repository. Digest is the
// chart's OCI manifest digest, or for an https:// repository the digest of that version in its index.yaml.
type Chart struct {
	Repo    string `yaml:"repo" json:"repo"`
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
	Digest  string `yaml:"digest" json:"digest"`
}

// Component is one chart the platform installs on one of its clusters.
type Component struct {
	Name        string   `yaml:"name" json:"name"`
	Cluster     string   `yaml:"cluster" json:"cluster"` // seed or workload
	Wave        int      `yaml:"wave" json:"wave"`       // applied and health-gated in ascending order per cluster
	Install     string   `yaml:"install" json:"install"` // application (the default) or helmchart
	Namespace   string   `yaml:"namespace" json:"namespace"`
	Chart       Chart    `yaml:"chart" json:"chart"`
	Group       string   `yaml:"group" json:"group"`       // workload node group: "" (system), all, system, kamaji, dbaas
	Stateful    bool     `yaml:"stateful" json:"stateful"` // prune off, never deleted by a run
	UpgradeFrom []string `yaml:"upgradeFrom,omitempty" json:"upgradeFrom,omitempty"`
	// Feature marks a component that is rendered only for an installation whose settings ask for it: FeatureSandbox,
	// for spec.sandbox. "" is every component there was before the field: always rendered. A marked component of our
	// own chart is also the one addition an upgrade makes to an installed platform (Classify).
	Feature string         `yaml:"feature,omitempty" json:"feature,omitempty"`
	Values  map[string]any `yaml:"values,omitempty" json:"values,omitempty"` // Helm values the component always gets
}

const (
	ClusterSeed        = "seed"
	ClusterWorkload    = "workload"
	InstallApplication = "application"
	InstallHelmChart   = "helmchart"
	pendingDigest      = "pending"
	ownRepo            = "ghcr.io/mutex-solutions/"
	mirrorRepo         = ownRepo + "charts-mirror" // third-party charts republished by CI (docs/ci-release.md, "Chart mirror")
	schemaVersion      = 1
	// FeatureSandbox is the feature of the sandbox chart's component (Component.Feature).
	FeatureSandbox = "sandbox"
)

// Feature is b's component marked with feature name, nil when it has none.
func (b *BOM) Feature(name string) *Component {
	for i := range b.Components {
		if b.Components[i].Feature == name {
			return &b.Components[i]
		}
	}
	return nil
}

// For is b as one installation renders it: without the components of a feature its settings do not ask for. The
// sandbox component is an Application of the platform cluster only with spec.sandbox shared; with separate the sandbox
// cluster's own chart installs that chart inside it (SandboxClusterValues), and without sandboxes nothing does. A BOM
// with no marked component, which is every BOM before the field, is returned as it is.
func (b *BOM) For(s Spec) *BOM {
	if b.Feature(FeatureSandbox) == nil || s.sandboxMode() == SandboxShared {
		return b
	}
	out := *b
	out.Components = slices.DeleteFunc(slices.Clone(b.Components), func(c Component) bool { return c.Feature != "" })
	return &out
}

// Parse decodes a BOM. Unknown keys are refused, so a misspelt key cannot drop a pin. It does not validate.
func Parse(raw []byte) (*BOM, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var b BOM
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("platform BOM: %w", err)
	}
	for i := range b.Components {
		if b.Components[i].Install == "" {
			b.Components[i].Install = InstallApplication
		}
	}
	return &b, nil
}

var (
	// exactVersion is one version, never a range: [v]MAJOR.MINOR.PATCH[-pre][+build].
	exactVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	movingZero   = regexp.MustCompile(`^v?0\.0\.0\+`) // 0.0.0+latest: a version that is republished in place
	sha256Digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	label        = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	workloadGrps = []string{"", "all", "system", "kamaji", "dbaas"}
)

// ownPackage: ref (a chart repo or an image name) is a package this repository builds and publishes at its own tags.
// The chart mirror is in our registry but holds third-party charts that exist before the BOM names them, so it is not.
func ownPackage(ref string) bool {
	return strings.HasPrefix(ref, ownRepo) && ref != mirrorRepo && !strings.HasPrefix(ref, mirrorRepo+"/")
}

// Validate checks the rules of the BOM and returns every problem it finds:
//   - every version is exact (no range, no latest);
//   - every chart and image has a digest;
//   - only a "-dev" release may hold a 0.0.0+ version or a "pending" digest, and "pending" only for our own packages
//     (ghcr.io/mutex-solutions/, without the chart mirror) or a chart at a 0.0.0+ version, which has no stable digest;
//   - component names are unique per cluster and waves ascend in the order components are listed.
func (b *BOM) Validate() error {
	dev := strings.HasSuffix(b.Release, "-dev")
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	version := func(field, v string) {
		switch {
		case !exactVersion.MatchString(v):
			bad("%s: %q is not an exact version", field, v)
		case movingZero.MatchString(v) && !dev:
			bad("%s: %s is allowed only in a -dev release", field, v)
		}
	}
	// mayPend: our own package (ownPackage), or a chart at a 0.0.0+ version.
	digest := func(field, d string, mayPend bool) {
		switch {
		case d == pendingDigest && !dev:
			bad("%s: digest pending is allowed only in a -dev release", field)
		case d == pendingDigest && !mayPend:
			bad("%s: digest pending is allowed only for our own packages and 0.0.0+ versions", field)
		case d != pendingDigest && !sha256Digest.MatchString(d):
			bad("%s: digest %q is not sha256:<64 hex>", field, d)
		}
	}
	chart := func(field string, c Chart) {
		if c.Repo == "" || (strings.Contains(c.Repo, "://") && !strings.HasPrefix(c.Repo, "https://")) {
			bad("%s: repo %q is neither a bare OCI repository nor https://", field, c.Repo)
		}
		if !label.MatchString(c.Name) {
			bad("%s: chart name %q", field, c.Name)
		}
		version(field+".version", c.Version)
		digest(field, c.Digest, ownPackage(c.Repo) || movingZero.MatchString(c.Version))
	}
	vmImage := func(field string, im VMImage) {
		if !label.MatchString(im.Package) || im.Tag == "" {
			bad("%s: package %q and tag %q are required", field, im.Package, im.Tag)
		}
		if !sha256Digest.MatchString(im.Digest) {
			bad("%s: digest %q is not sha256:<64 hex>", field, im.Digest)
		}
	}
	image := func(field, ref string) {
		name, d, _ := strings.Cut(ref, "@")
		i := strings.LastIndex(name, ":")
		if i <= 0 || strings.Contains(name[i:], "/") {
			bad("%s: %q is not <repo>:<tag>@<digest>", field, ref)
			return
		}
		version(field+" tag", name[i+1:])
		digest(field, d, ownPackage(name))
	}
	providers := func(field string, p Providers) {
		for _, f := range []struct{ key, ref string }{{"core", p.Core}, {"bootstrap", p.Bootstrap},
			{"controlPlane", p.ControlPlane}, {"infrastructure", p.Infrastructure}, {"ipam", p.IPAM}, {"addon", p.Addon}} {
			if f.ref == "" && (f.key == "ipam" || f.key == "addon") {
				continue
			}
			name, ver, _ := strings.Cut(f.ref, ":")
			if !label.MatchString(name) {
				bad("%s.%s: %q is not <provider>:<version>", field, f.key, f.ref)
				continue
			}
			version(field+"."+f.key, ver)
		}
	}

	if b.SchemaVersion != schemaVersion {
		bad("schemaVersion %d is not %d", b.SchemaVersion, schemaVersion)
	}
	version("release", b.Release)
	if strings.HasPrefix(b.Release, "v") {
		bad("release %q: no v prefix", b.Release)
	}
	version("kubernetes.version", b.Kubernetes.Version)
	vmImage("kubernetes.nodeImage", b.Kubernetes.NodeImage)
	if !strings.Contains(b.Kubernetes.NodeImage.Tag, "-k8s-"+b.Kubernetes.Version+"-") {
		bad("kubernetes.nodeImage: tag %q is not a %s node image", b.Kubernetes.NodeImage.Tag, b.Kubernetes.Version)
	}
	vmImage("seed.image", b.Seed.Image)
	version("seed.k3s.version", b.Seed.K3s.Version)
	for _, f := range []struct {
		field string
		d     Download
	}{{"seed.k3s.binary", b.Seed.K3s.Binary}, {"seed.k3s.installScript", b.Seed.K3s.InstallScript}} {
		field, d := f.field, f.d
		if !strings.HasPrefix(d.URL, "https://") || !strings.Contains(d.URL, "/"+strings.ReplaceAll(b.Seed.K3s.Version, "+", "%2B")+"/") {
			bad("%s: url %q is not https:// for k3s %s", field, d.URL, b.Seed.K3s.Version)
		}
		if !sha256Hex.MatchString(d.SHA256) {
			bad("%s: sha256 %q is not 64 hex digits", field, d.SHA256)
		}
	}
	image("images.kubeVip", b.Images.KubeVip)
	image("images.clusterctl", b.Images.Clusterctl)
	providers("capi.seed", b.CAPI.Seed)
	providers("capi.workload", b.CAPI.Workload)
	chart("tenantCharts.kubernetes", b.TenantCharts.Kubernetes)
	chart("tenantCharts.database", b.TenantCharts.Database)

	seen := map[string]bool{}
	lastWave := map[string]int{}
	for i, c := range b.Components {
		field := fmt.Sprintf("components[%d] %s/%s", i, c.Cluster, c.Name)
		if c.Cluster != ClusterSeed && c.Cluster != ClusterWorkload {
			bad("%s: cluster %q is neither seed nor workload", field, c.Cluster)
		}
		if !label.MatchString(c.Name) || !label.MatchString(c.Namespace) {
			bad("%s: name %q and namespace %q must be DNS labels", field, c.Name, c.Namespace)
		}
		if seen[c.Cluster+"/"+c.Name] {
			bad("%s: name is not unique in its cluster", field)
		}
		seen[c.Cluster+"/"+c.Name] = true
		if last, ok := lastWave[c.Cluster]; c.Wave < 0 || (ok && c.Wave < last) {
			bad("%s: wave %d is negative or below the wave before it", field, c.Wave)
		}
		lastWave[c.Cluster] = c.Wave
		switch {
		case c.Install != InstallApplication && c.Install != InstallHelmChart:
			bad("%s: install %q is neither application nor helmchart", field, c.Install)
		case c.Install == InstallHelmChart && c.Cluster != ClusterSeed:
			bad("%s: install helmchart is only for the seed", field)
		}
		if (c.Cluster == ClusterSeed && c.Group != "") || !slices.Contains(workloadGrps, c.Group) {
			bad("%s: group %q", field, c.Group)
		}
		// A marked component is skipped for most installations: nothing may depend on it being there, so it is a
		// stateless chart of our own on the workload cluster, placed by its own values (group "").
		if c.Feature != "" && (c.Feature != FeatureSandbox || !ownPackage(c.Chart.Repo) || c.Cluster != ClusterWorkload || c.Group != "" || c.Stateful || c.Install != InstallApplication) {
			bad("%s: feature %q: only %s, on an application of our own chart for the workload cluster, without a group and not stateful", field, c.Feature, FeatureSandbox)
		}
		chart(field, c.Chart)
		for _, v := range c.UpgradeFrom {
			version(field+".upgradeFrom", v)
		}
	}
	return errors.Join(errs...)
}

// Embedded is a BOM with the sha256 of the file it was parsed from, the key a run records it by.
type Embedded struct {
	SHA256 string `json:"sha256"`
	*BOM
}

// Current is the BOM this build embeds, parsed and validated once. bom_test.go keeps it valid.
var Current = sync.OnceValues(func() (Embedded, error) {
	sum := sha256.Sum256(deploy.PlatformBOM)
	b, err := Parse(deploy.PlatformBOM)
	if err == nil {
		err = b.Validate()
	}
	return Embedded{SHA256: hex.EncodeToString(sum[:]), BOM: b}, err
})

// Previous are the BOMs of earlier releases this build embeds next to its own (deploy/platform/previous/): a platform
// one of them installed keeps its applies, which render from that BOM and not from the current one. None is a "-dev"
// BOM, so a platform installed by a build of main is not among them. bom_test.go keeps them valid, unchanged and
// limited to the releases it lists.
var Previous = sync.OnceValues(func() ([]Embedded, error) {
	files, err := fs.Glob(deploy.PreviousBOMs, "platform/previous/*.yaml")
	if err != nil {
		return nil, err
	}
	var out []Embedded
	for _, f := range files {
		raw, err := deploy.PreviousBOMs.ReadFile(f)
		if err != nil {
			return nil, err
		}
		b, err := Parse(raw)
		if err == nil {
			err = b.Validate()
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, Embedded{SHA256: hex.EncodeToString(sum[:]), BOM: b})
	}
	return out, nil
})

// Lookup finds a BOM this build embeds by the sha256 of its file: the current one or a previous one.
func Lookup(sha string) (Embedded, bool) {
	if cur, err := Current(); err == nil && cur.SHA256 == sha {
		return cur, true
	}
	prev, _ := Previous()
	for _, b := range prev {
		if b.SHA256 == sha {
			return b, true
		}
	}
	return Embedded{}, false
}

// bomOf is the BOM an installation's images, charts and namespaces come from: the installed one when this build embeds
// it, else the current one (a new installation, or one whose apply is refused with bom_unsupported).
func bomOf(in *Installation) (*BOM, error) {
	if ref := in.Status.BOM; ref != nil {
		if b, ok := Lookup(ref.SHA256); ok {
			return b.BOM.For(in.Spec), nil
		}
	}
	cur, err := Current()
	if err != nil {
		return cur.BOM, err
	}
	return cur.BOM.For(in.Spec), nil
}

// blockedAdvice ends the message about a BOM image on the template update blocklist. A newer release brings other
// images only to an installation that has no recorded BOM yet: an installed platform keeps its BOM across upgrades.
func blockedAdvice(in *Installation) string {
	if ref := in.Status.BOM; ref != nil {
		return "the platform stays on release " + ref.Release + " and its images, and no ProxCloud release moves an installed platform to other images yet"
	}
	return "install a newer ProxCloud release"
}
