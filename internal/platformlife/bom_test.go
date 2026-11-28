package platformlife

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mutex-solutions/proxcloud/deploy"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
)

func embedded(t *testing.T) *BOM {
	t.Helper()
	b, err := Parse(deploy.PlatformBOM)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// plain is this build's BOM as an installation without sandboxes renders it (BOM.For): every component but the sandbox
// chart's, which the fixtures of the render tests do not ask for.
func plain(t *testing.T) *BOM {
	t.Helper()
	return embedded(t).For(Spec{})
}

func comp(t *testing.T, b *BOM, cluster, name string) *Component {
	t.Helper()
	for i := range b.Components {
		if b.Components[i].Cluster == cluster && b.Components[i].Name == name {
			return &b.Components[i]
		}
	}
	t.Fatalf("the BOM has no %s component %s", cluster, name)
	return nil
}

func TestCurrentIsValid(t *testing.T) {
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if len(cur.SHA256) != 64 || len(cur.Components) == 0 {
		t.Fatalf("release %q, sha256 %q, %d components", cur.Release, cur.SHA256, len(cur.Components))
	}
	if c := comp(t, cur.BOM, ClusterWorkload, "cilium"); c.Install != InstallApplication {
		t.Errorf("an entry without install is %q, want application", c.Install)
	}
}

// The four package versions this repository has published stay pinned by digest. A newer version of one of them may
// be `pending` in a -dev BOM until its own tag has published it (Validate, mayPend). The third-party charts come from
// the chart mirror and are never pending (TestValidate); .github/scripts/bom_verify.py --release refuses every
// `pending` at a v* tag.
func TestPublishedPackagesArePinned(t *testing.T) {
	b := embedded(t)
	if strings.HasSuffix(b.Images.Clusterctl, "/proxcloud-clusterctl:v1.14.4-1@pending") {
		t.Errorf("images.clusterctl %s is pending", b.Images.Clusterctl)
	}
	published := map[string]string{"proxmox-kamaji-cluster": "0.1.19", "database-cluster": "0.5.2", "proxcloud-platform": "0.1.3"}
	charts := []Chart{b.TenantCharts.Kubernetes, b.TenantCharts.Database}
	for _, c := range b.Components {
		charts = append(charts, c.Chart)
	}
	for _, c := range charts {
		if c.Digest == "pending" && ownPackage(c.Repo) && published[c.Name] == c.Version {
			t.Errorf("our published chart %s %s is pending", c.Name, c.Version)
		}
	}
}

func TestParseRefusesUnknownKeys(t *testing.T) {
	for name, raw := range map[string]string{
		"top level": string(deploy.PlatformBOM) + "\nlatest: true\n",
		"component": strings.Replace(string(deploy.PlatformBOM), "stateful: true", "statefull: true", 1),
	} {
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidate(t *testing.T) {
	const sha = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name   string
		mutate func(t *testing.T, b *BOM)
		want   []string // substrings of the error; none = valid
	}{
		{"the embedded BOM", func(*testing.T, *BOM) {}, nil},
		{"a caret range", func(t *testing.T, b *BOM) { comp(t, b, "workload", "cilium").Chart.Version = "^1.18.16" }, []string{"not an exact version"}},
		{"latest", func(t *testing.T, b *BOM) { comp(t, b, "workload", "cilium").Chart.Version = "latest" }, []string{"not an exact version"}},
		{"two parts", func(t *testing.T, b *BOM) { comp(t, b, "workload", "cilium").Chart.Version = "1.18" }, []string{"not an exact version"}},
		{"a wildcard", func(t *testing.T, b *BOM) { comp(t, b, "workload", "cilium").Chart.Version = "1.18.x" }, []string{"not an exact version"}},
		{"a comparison", func(t *testing.T, b *BOM) { b.Kubernetes.Version = ">=1.35.10" }, []string{"kubernetes.version", "not an exact version"}},
		{"a release keeps no pending digest and no 0.0.0+ version", func(t *testing.T, b *BOM) {
			b.Release = "0.7.0"
			comp(t, b, "workload", "kamaji").Chart.Version = "0.0.0+latest"               // as before the chart mirror
			b.Images.Clusterctl = strings.Split(b.Images.Clusterctl, "@")[0] + "@pending" // as before its first publication
			b.TenantCharts.Kubernetes.Digest = "pending"
		},
			[]string{"tenantCharts.kubernetes: digest pending is allowed only in a -dev release", "images.clusterctl: digest pending",
				"workload/kamaji.version: 0.0.0+latest is allowed only in a -dev release"}},
		{"a release with everything pinned", func(t *testing.T, b *BOM) {
			b.Release = "0.7.0"
			// A pre-release version and a v-prefixed one are exact versions, in a release too.
			if k, c := comp(t, b, "workload", "kamaji").Chart.Version, comp(t, b, "workload", "cert-manager").Chart.Version; k != "26.11.2-edge" || c != "v1.21.3" {
				t.Fatalf("kamaji %s, cert-manager %s", k, c)
			}
			b.Images.Clusterctl = strings.Replace(b.Images.Clusterctl, "@pending", "@"+sha, 1)
			b.TenantCharts.Kubernetes.Digest, b.TenantCharts.Database.Digest = sha, sha
			for i := range b.Components {
				if c := &b.Components[i].Chart; c.Digest == "pending" {
					c.Digest = sha
				}
				if c := &b.Components[i].Chart; strings.HasPrefix(c.Version, "0.0.0+") {
					c.Version = "1.0.0"
				}
			}
		}, nil},
		// The chart mirror is in our registry and is not our own package: its charts are third-party.
		{"pending on a chart of the mirror", func(t *testing.T, b *BOM) {
			c := comp(t, b, "workload", "cilium")
			if c.Chart.Repo != mirrorRepo {
				t.Fatalf("cilium comes from %s, not from the mirror", c.Chart.Repo)
			}
			c.Chart.Digest = "pending"
		}, []string{"workload/cilium: digest pending is allowed only for our own packages"}},
		{"pending on a chart below the mirror path", func(t *testing.T, b *BOM) {
			c := comp(t, b, "workload", "cilium")
			c.Chart.Repo, c.Chart.Digest = mirrorRepo+"/sub", "pending"
		}, []string{"workload/cilium: digest pending is allowed only for our own packages"}},
		{"pending on a third-party chart", func(t *testing.T, b *BOM) {
			c := comp(t, b, "workload", "cilium")
			c.Chart.Repo, c.Chart.Digest = "https://helm.cilium.io", "pending"
		}, []string{"workload/cilium: digest pending is allowed only for our own packages"}},
		{"pending on our own chart in a -dev release", func(t *testing.T, b *BOM) { comp(t, b, "seed", "capi").Chart.Digest = "pending" }, nil},
		{"pending on our own repo whose name starts like the mirror", func(t *testing.T, b *BOM) {
			c := comp(t, b, "seed", "capi")
			c.Chart.Repo, c.Chart.Digest = mirrorRepo+"-own", "pending"
		}, nil},
		{"a 0.0.0+ version with a pending digest in a -dev release", func(t *testing.T, b *BOM) {
			c := comp(t, b, "workload", "kamaji")
			c.Chart.Repo, c.Chart.Version, c.Chart.Digest = "https://clastix.github.io/charts", "0.0.0+latest", "pending"
		}, nil},
		{"pending on a third-party image", func(_ *testing.T, b *BOM) {
			b.Images.KubeVip = strings.Split(b.Images.KubeVip, "@")[0] + "@pending"
		}, []string{"images.kubeVip: digest pending is allowed only for our own packages"}},
		{"a chart without digest", func(t *testing.T, b *BOM) { comp(t, b, "workload", "metallb").Chart.Digest = "" }, []string{"workload/metallb: digest \"\""}},
		{"a short digest", func(t *testing.T, b *BOM) { comp(t, b, "workload", "metallb").Chart.Digest = "sha256:96667a81" }, []string{"not sha256:<64 hex>"}},
		{"an image without digest", func(_ *testing.T, b *BOM) { b.Images.KubeVip = "ghcr.io/kube-vip/kube-vip:v0.8.9" }, []string{"images.kubeVip: digest \"\""}},
		{"an image without tag", func(_ *testing.T, b *BOM) { b.Images.KubeVip = "ghcr.io/kube-vip/kube-vip@" + sha }, []string{"not <repo>:<tag>@<digest>"}},
		{"an image at latest", func(_ *testing.T, b *BOM) { b.Images.KubeVip = "ghcr.io/kube-vip/kube-vip:latest@" + sha }, []string{"images.kubeVip tag"}},
		{"a pending VM image", func(_ *testing.T, b *BOM) { b.Seed.Image.Digest = "pending" }, []string{"seed.image: digest"}},
		{"a node image of another version", func(_ *testing.T, b *BOM) { b.Kubernetes.Version = "1.36.7" }, []string{"is not a 1.36.7 node image"}},
		{"k3s downloads of another version", func(_ *testing.T, b *BOM) { b.Seed.K3s.Version = "v1.35.11+k3s1" },
			[]string{"seed.k3s.binary: url", "seed.k3s.installScript: url"}},
		{"a k3s sha256 that is a digest", func(_ *testing.T, b *BOM) { b.Seed.K3s.Binary.SHA256 = sha }, []string{"seed.k3s.binary: sha256"}},
		{"a provider without version", func(_ *testing.T, b *BOM) { b.CAPI.Seed.Core = "cluster-api" }, []string{"capi.seed.core"}},
		{"a provider at a range", func(_ *testing.T, b *BOM) { b.CAPI.Workload.Infrastructure = "proxmox:~0.9" }, []string{"capi.workload.infrastructure"}},
		{"no ipam and no addon", func(_ *testing.T, b *BOM) { b.CAPI.Workload.IPAM, b.CAPI.Workload.Addon = "", "" }, nil},
		{"a required provider missing", func(_ *testing.T, b *BOM) { b.CAPI.Workload.ControlPlane = "" }, []string{"capi.workload.controlPlane"}},
		{"a name twice in one cluster", func(t *testing.T, b *BOM) { comp(t, b, "workload", "metallb").Name = "cilium" }, []string{"not unique"}},
		{"a wave below the one before it", func(t *testing.T, b *BOM) { comp(t, b, "workload", "kamaji").Wave = 1 }, []string{"workload/kamaji: wave 1"}},
		{"a negative wave", func(t *testing.T, b *BOM) { comp(t, b, "workload", "cilium").Wave = -1 }, []string{"wave -1"}},
		{"an upgradeFrom range", func(t *testing.T, b *BOM) {
			comp(t, b, "workload", "cilium").UpgradeFrom = []string{"1.18.15", ">=1.17.0"}
		}, []string{"workload/cilium.upgradeFrom: \">=1.17.0\""}},
		{"a HelmChart on the workload", func(t *testing.T, b *BOM) { comp(t, b, "workload", "argocd").Install = InstallHelmChart }, []string{"only for the seed"}},
		{"an unknown install", func(t *testing.T, b *BOM) { comp(t, b, "workload", "argocd").Install = "kustomize" }, []string{"neither application nor helmchart"}},
		{"an unknown cluster", func(t *testing.T, b *BOM) { comp(t, b, "workload", "argocd").Cluster = "edge" }, []string{"neither seed nor workload"}},
		{"a seed component with a group", func(t *testing.T, b *BOM) { comp(t, b, "seed", "argocd").Group = "system" }, []string{"seed/argocd: group"}},
		{"a group outside the M1 set", func(t *testing.T, b *BOM) { comp(t, b, "workload", "cilium").Group = "sandbox" }, []string{"group \"sandbox\""}},
		{"an http repo", func(t *testing.T, b *BOM) { comp(t, b, "workload", "cilium").Chart.Repo = "http://helm.cilium.io" }, []string{"neither a bare OCI repository nor https://"}},
		{"an oci:// repo", func(t *testing.T, b *BOM) {
			comp(t, b, "workload", "argocd").Chart.Repo = "oci://ghcr.io/argoproj/argo-helm"
		}, []string{"neither a bare OCI"}},
		{"schema 2", func(_ *testing.T, b *BOM) { b.SchemaVersion = 2 }, []string{"schemaVersion 2"}},
		{"a release candidate is a release: no pending digest", func(_ *testing.T, b *BOM) {
			b.Release = "0.9.0-rc.1"
			b.TenantCharts.Kubernetes.Digest = "pending"
		}, []string{"tenantCharts.kubernetes: digest pending is allowed only in a -dev release"}},
		{"a release candidate with everything pinned", func(_ *testing.T, b *BOM) { b.Release = "0.9.0-rc.1" }, nil},
		{"a release with v", func(_ *testing.T, b *BOM) { b.Release = "v0.7.0-dev" }, []string{"no v prefix"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := embedded(t)
			// The pending and 0.0.0+ cases are rules of a -dev release, whatever the embedded BOM is at this commit
			// (it has no -dev at a release commit). The first case validates the file as it is.
			if tc.name != "the embedded BOM" {
				b.Release = "0.9.0-dev"
			}
			tc.mutate(t, b)
			err := b.Validate()
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

func readYAML(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// The installer pins the same versions (installer/playbooks/group_vars/all/versions.yml and pc_defaults), for every
// component both name, but k3s and Kubernetes: those two may stay one minor behind the BOM and are never ahead of it.
// `make upgrade` reinstalls k3s in place on a multi-server management cluster that runs Traefik and lets plain Cluster
// API roll the services and sandbox clusters, with none of the upgrade's gates, so the installer path takes a minor
// step in a release of its own. The kubectl in the clusterctl image follows pc_defaults.services.k8s_version
// (deploy/tests/test_clusterctl_image.py) and is supported one minor from the API server, which is the bound here.
// barman_plugin is not compared: backups are not in the BOM until M2.
func TestBOMMatchesInstallerVersions(t *testing.T) {
	b := embedded(t)
	var vf struct {
		V map[string]any `yaml:"pc_versions"`
	}
	readYAML(t, "../../installer/playbooks/group_vars/all/versions.yml", &vf)
	var df struct {
		D struct {
			Services struct {
				K8sVersion string `yaml:"k8s_version"`
			} `yaml:"services"`
		} `yaml:"pc_defaults"`
	}
	readYAML(t, "../../installer/playbooks/group_vars/all/defaults.yml", &df)

	chart := func(cluster, name string) string { return comp(t, b, cluster, name).Chart.Version }
	ver := func(provider string) string { _, v, _ := strings.Cut(provider, ":"); return v }
	tag := func(ref string) string {
		name, _, _ := strings.Cut(ref, "@")
		return name[strings.LastIndex(name, ":")+1:]
	}
	// The mirrored kamaji charts are packaged with version = appVersion = the image tag, and the chart's image falls
	// back to appVersion, so the BOM sets no image.tag and the chart version is the image pin.
	if tag := dig(comp(t, b, ClusterWorkload, "kamaji").Values, "image", "tag"); tag != nil {
		t.Errorf("workload kamaji sets image.tag %v; the mirrored chart's appVersion is the pin", tag)
	}

	for _, p := range []struct{ key, field, bom string }{
		{"argocd_chart", "seed argocd", chart(ClusterSeed, "argocd")},
		{"argocd_chart", "workload argocd", chart(ClusterWorkload, "argocd")},
		{"cert_manager_chart", "seed cert-manager", chart(ClusterSeed, "cert-manager")},
		{"cert_manager_chart", "workload cert-manager", chart(ClusterWorkload, "cert-manager")},
		{"cnpg_chart", "workload cnpg", chart(ClusterWorkload, "cnpg")},
		{"capi", "capi.seed.core", ver(b.CAPI.Seed.Core)},
		{"capi", "capi.seed.bootstrap", ver(b.CAPI.Seed.Bootstrap)},
		{"capi", "capi.seed.controlPlane", ver(b.CAPI.Seed.ControlPlane)},
		{"capi", "capi.workload.core", ver(b.CAPI.Workload.Core)},
		{"capi", "capi.workload.bootstrap", ver(b.CAPI.Workload.Bootstrap)},
		{"capmox", "capi.seed.infrastructure", ver(b.CAPI.Seed.Infrastructure)},
		{"capmox", "capi.workload.infrastructure", ver(b.CAPI.Workload.Infrastructure)},
		{"capi_ipam", "capi.seed.ipam", ver(b.CAPI.Seed.IPAM)},
		{"capi_ipam", "capi.workload.ipam", ver(b.CAPI.Workload.IPAM)},
		{"capi_kamaji", "capi.workload.controlPlane", ver(b.CAPI.Workload.ControlPlane)},
		{"caaph", "capi.workload.addon", ver(b.CAPI.Workload.Addon)},
		{"kube_vip", "images.kubeVip", tag(b.Images.KubeVip)},
		{"cilium_chart", "workload cilium", chart(ClusterWorkload, "cilium")},
		{"metallb_chart", "workload metallb", chart(ClusterWorkload, "metallb")},
		{"proxmox_csi_chart", "workload proxmox-csi", chart(ClusterWorkload, "proxmox-csi")},
		{"kamaji_etcd_chart", "workload kamaji-etcd", chart(ClusterWorkload, "kamaji-etcd")},
		{"kamaji_image", "workload kamaji", chart(ClusterWorkload, "kamaji")},
		{"kamaji_image", "workload kamaji-crds", chart(ClusterWorkload, "kamaji-crds")},
	} {
		if got := fmt.Sprint(vf.V[p.key]); got != p.bom {
			t.Errorf("versions.yml %s = %s, BOM %s = %q", p.key, got, p.field, p.bom)
		}
	}
	if why := installerBehind(fmt.Sprint(vf.V["k3s"]), b.Seed.K3s.Version); why != "" {
		t.Errorf("versions.yml k3s = %v, BOM seed.k3s.version = %s: %s", vf.V["k3s"], b.Seed.K3s.Version, why)
	}
	if why := installerBehind(df.D.Services.K8sVersion, b.Kubernetes.Version); why != "" {
		t.Errorf("pc_defaults.services.k8s_version = %s, BOM kubernetes.version = %s: %s", df.D.Services.K8sVersion, b.Kubernetes.Version, why)
	}
	for _, c := range []struct{ installer, bom, want string }{
		{"1.36.6", "1.36.6", ""}, {"1.35.10", "1.36.6", ""}, {"v1.35.10+k3s1", "v1.36.6+k3s1", ""}, {"1.36.5", "1.36.6", ""},
		{"1.36.7", "1.36.6", "newer than the BOM"}, {"v1.36.6+k3s2", "v1.36.6+k3s1", "newer than the BOM"}, {"1.37.0", "1.36.6", "newer than the BOM"},
		{"1.34.9", "1.36.6", "more than one minor behind the BOM"}, {"0.36.6", "1.36.6", "more than one minor behind the BOM"}, {"", "1.36.6", "not a version"},
	} {
		if got := installerBehind(c.installer, c.bom); (got == "") != (c.want == "") || !strings.Contains(got, c.want) {
			t.Errorf("installer %q against BOM %q: %q, want %q", c.installer, c.bom, got, c.want)
		}
	}
}

// installerBehind is why an installer pin of k3s or Kubernetes does not fit the BOM's ("" when it does): it is the
// BOM's version or an older one, at most one minor back.
func installerBehind(installer, bom string) string {
	parse := func(v string) ([]int, error) {
		core, build, _ := strings.Cut(v, "+k3s")
		major, minor, patch, err := kamaji.ParseVersion(core)
		n, _ := strconv.Atoi(build)
		return []int{major, minor, patch, n}, err
	}
	i, err := parse(installer)
	if err != nil {
		return "not a version: " + err.Error()
	}
	b, err := parse(bom)
	switch {
	case err != nil:
		return "not a version: " + err.Error()
	case slices.Compare(i, b) > 0:
		return "newer than the BOM"
	case i[0] != b[0] || i[1] < b[1]-1:
		return "more than one minor behind the BOM"
	}
	return ""
}

// stage40 renders a stage 40 template with the installer's versions: the templates only substitute pc_ver keys.
func stage40(t *testing.T, name string) []map[string]any {
	t.Helper()
	var vf struct {
		V map[string]any `yaml:"pc_versions"`
	}
	readYAML(t, "../../installer/playbooks/group_vars/all/versions.yml", &vf)
	raw, err := os.ReadFile("../../installer/playbooks/templates/" + name)
	if err != nil {
		t.Fatal(err)
	}
	text := regexp.MustCompile(`\{\{ pc_ver\.(\w+) \}\}`).ReplaceAllStringFunc(string(raw), func(m string) string {
		return fmt.Sprint(vf.V[m[len("{{ pc_ver."):len(m)-len(" }}")]])
	})
	if strings.Contains(text, "{{") {
		t.Fatalf("%s has template expressions this test does not render", name)
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	for {
		var d map[string]any
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		docs = append(docs, d)
	}
	return docs
}

// The seed's ArgoCD and cert-manager are installed as stage 40 installs them on the installer's mgmt cluster: same
// chart, version and values.
func TestBOMMatchesStage40(t *testing.T) {
	b := embedded(t)

	hc := stage40(t, "argocd-helmchart.yml.j2")
	argocd := comp(t, b, ClusterSeed, "argocd")
	if len(hc) != 1 {
		t.Fatalf("argocd-helmchart.yml.j2: %d documents", len(hc))
	}
	if got, want := dig(hc[0], "spec", "chart"), "oci://"+argocd.Chart.Repo+"/"+argocd.Chart.Name; got != want {
		t.Errorf("HelmChart chart %v, BOM %s", got, want)
	}
	if got := fmt.Sprint(dig(hc[0], "spec", "version")); got != argocd.Chart.Version {
		t.Errorf("HelmChart version %s, BOM %s", got, argocd.Chart.Version)
	}
	var values map[string]any
	if err := yaml.Unmarshal([]byte(fmt.Sprint(dig(hc[0], "spec", "valuesContent"))), &values); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(values, argocd.Values) {
		t.Errorf("HelmChart valuesContent %v, BOM values %v", values, argocd.Values)
	}

	cm := comp(t, b, ClusterSeed, "cert-manager")
	var app map[string]any
	for _, d := range stage40(t, "apps-operators.yml.j2") {
		if dig(d, "metadata", "name") == "cert-manager" {
			app = d
		}
	}
	if app == nil {
		t.Fatal("apps-operators.yml.j2 has no cert-manager Application")
	}
	src := dig(app, "spec", "source")
	for _, f := range []struct{ key, want string }{{"repoURL", cm.Chart.Repo}, {"chart", cm.Chart.Name}, {"targetRevision", cm.Chart.Version}} {
		if got := fmt.Sprint(dig(src, f.key)); got != f.want {
			t.Errorf("cert-manager Application %s %s, BOM %s", f.key, got, f.want)
		}
	}
	if got := dig(src, "helm", "valuesObject"); !reflect.DeepEqual(got, cm.Values) {
		t.Errorf("cert-manager Application values %v, BOM values %v", got, cm.Values)
	}
}

// The two VM images are rows of the published images table, digest and tag on the same row.
func TestBOMImagesArePublished(t *testing.T) {
	b := embedded(t)
	raw, err := os.ReadFile("../../docs/vm-artifact-publishing.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, _ := strings.Cut(string(raw), "## Published images")
	for _, im := range []VMImage{b.Kubernetes.NodeImage, b.Seed.Image} {
		ref := "`ghcr.io/mutex-solutions/" + im.Package + "@" + im.Digest + "` | `" + im.Tag + "`"
		if !strings.Contains(table, ref) {
			t.Errorf("docs/vm-artifact-publishing.md does not list %s", ref)
		}
	}
}

// previousSHA256 is the sha256 of every previous BOM this build embeds, as its release published it. The one of 0.8.0
// is the sha256 of deploy/platform/platform-bom.yaml at the tag v0.8.0 (git show v0.8.0:deploy/platform/platform-bom.yaml
// | sha256sum), which is what a platform installed by v0.8.0 recorded in status.bom. The same holds for each later one.
var previousSHA256 = map[string]string{
	"0.8.0":       "8eb874987d3bbaf55f309295ee241ec320c0cf9e0805b3bd1812ef908b6600c6",
	"0.8.1":       "a635bd88d3c39baf5519344088a6fed8dad8cb2b3c2d324655753e8ec55719b3",
	"0.8.2":       "a10baf0bef4f33ebff26cb87315259a6cf886c8ffe00b79dc24e2bed4ed6540b",
	"0.9.0-rc.1":  "731ffb6217aefd5dcca55085d8de47f60968afa37ec951830b6d3eb50bc75af8",
	"0.9.0-rc.2":  "bd512541501290813d841c7ac0338ca59a41c86768b3856b51f90db20dcc7808",
	"0.9.0":       "ca7140045f0bf3951b66a371b22a683ab54ec063ccf9b73562285f4238aa442d",
	"0.9.1":       "a9bbf835402a2fd4e13992ff389856c5be527498588f8e0a6bb0760a664ca2cd",
	"0.10.0-rc.1": "d43dc17287bebc9ea097acbcc4f3db9a37f1957eeed488ce071b6de5c1dd0412",
	"0.10.0":      "721b9992f2663c033cae4eed431511686c6178e3ea520c61ea3945aa07199817",
	"0.10.1-rc.1": "3065954af13509552321c91c693a07e45bfa767344f39a9c0b890d27297b0621",
	"0.10.1-rc.2": "14acba7b0d7ae05bda488066eb2f1699f73139bc78ae44df824a758e587d17bd",
	"0.10.1":      "aec05ea60e6bfd12d73bab3d45a6a2aae8a3f62f3a41d3f43737aa8ef1d74e3b",
	"0.10.2-rc.1": "76140de70df83ee767c07b7b2e63893feaf2ef4bed95b4cbdf58ba8973e38a6a",
	"0.10.2":      "78dbc9a79150ea71d77d460e8679ba94d1d7484c82e6f6bdcbcbf4560cdf3dee",
	"0.11.0-rc.1": "ca2313a651ccc9c99b37fd1c6d54d1ce09eb9220b3be74c4a70aa930f740a26d",
	"0.11.0-rc.2": "1c31634d18e5888d0974cf34e6b06c7bc6563c0d9035b326cc0e70ac41e72705",
	"0.11.0":      "d8e37db2fd204a6a58a4d6357f4c54bb6dccc82de52430340f2c8c7ca0e0d7ff",
	"0.11.1-rc.1": "2d96e6c616ec26f0263e94b190f7d10936645fa6d184bb789d9870518b4248ee",
	"0.11.1-rc.2": "b66f6e7f3f96efa724393abc2f070fedbf4c2fcf1ef79ce6411d933a38944073",
	"0.11.1-rc.3": "b9d119c59162ce75956bfb43a251915aaac6d8eee577944292be5434bbe3d581",
	"0.11.1-rc.4": "60c0281bd9723cb15585e92cc9087e05680798943c538d95044af77257f51222",
	"0.11.1-rc.5": "a28b54343f999674a2ca0e53b255312845cb190a78f5570dbaeaadcc7e8d9000",
	"0.11.1":      "b04cdabce8ffd004309fbb2e34ffe1a9871648db103553b4badc5bd17be69172",
	"0.12.0-rc.1": "19671c44e78f4888cb06a277bfd1a655b7d3622bb7f8e640f9c6b256f47479a1",
	"0.12.0-rc.2": "f45d5dd6e516798a5d178b01f81dd74dea4ec6df409eab1ceae8b99fd72c48a5",
}

// A previous BOM is the file of its tag, byte for byte: an installed platform names it by this sha256, so a changed
// byte (a comment too) would refuse every apply on that platform with bom_unsupported.
func TestPreviousBOMsAreUnchanged(t *testing.T) {
	prev, err := Previous()
	if err != nil {
		t.Fatal(err)
	}
	if len(prev) != len(previousSHA256) {
		t.Errorf("%d previous BOMs are embedded, previousSHA256 lists %d", len(prev), len(previousSHA256))
	}
	cur, _ := Current()
	for _, b := range prev {
		if want := previousSHA256[b.Release]; b.SHA256 != want {
			t.Errorf("deploy/platform/previous/%s.yaml has sha256 %s, its release published %q", b.Release, b.SHA256, want)
		}
		if got, ok := Lookup(b.SHA256); !ok || got.BOM != b.BOM || b.SHA256 == cur.SHA256 {
			t.Errorf("Lookup does not find the previous BOM %s, or it is the current one", b.Release)
		}
	}
	if _, ok := Lookup(strings.Repeat("0", 64)); ok {
		t.Error("Lookup found a BOM nobody embedded")
	}
}

// Every release from v0.8.0 on (the first with a platform) that CHANGELOG.md lists has its BOM under
// deploy/platform/previous/, except the release this BOM is: the change that follows a release adds the file
// (docs/ci-release.md, "Previous BOMs"). Without it the next release would refuse the platforms of that one. A release
// candidate (X.Y.Z-rc.N) counts: its BOM is a release BOM, a platform can be moved to it, and only this file lets the
// builds after it apply that platform.
func TestEveryReleaseHasItsPreviousBOM(t *testing.T) {
	changelog, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := Current()
	releases := regexp.MustCompile(`(?m)^## \[(\d+)\.(\d+)\.(\d+(?:-rc\.\d+)?)\]`).FindAllStringSubmatch(string(changelog), -1)
	if len(releases) == 0 {
		t.Fatal("CHANGELOG.md lists no release")
	}
	for _, m := range releases {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		release := m[1] + "." + m[2] + "." + m[3]
		if (major == 0 && minor < 8) || release == cur.Release {
			continue
		}
		if previousSHA256[release] == "" {
			t.Errorf("release %s has no deploy/platform/previous/%s.yaml", release, release)
		}
	}
}

// newerBuild makes the build under test one whose own BOM differs from 0.8.0's in every kind of pin a run reads (the
// BOM on main may differ by its release line only), and answers the embedded 0.8.0 BOM.
func newerBuild(t *testing.T) Embedded {
	t.Helper()
	cur, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	saved := *cur.BOM
	t.Cleanup(func() { *cur.BOM = saved })
	cur.Release = "9.9.9-dev"
	cur.Seed.Image.Digest = "sha256:" + strings.Repeat("a", 64)
	cur.Kubernetes.NodeImage.Digest = "sha256:" + strings.Repeat("b", 64)
	cur.TenantCharts.Kubernetes.Version, cur.TenantCharts.Database.Version = "9.9.9", "9.9.9"
	cur.Components = slices.Clone(saved.Components)
	for i := range cur.Components {
		cur.Components[i].Chart.Version = "9.9.9"
		if cur.Components[i].Cluster == ClusterWorkload {
			cur.Components[i].Namespace = "moved"
		}
	}
	old, ok := Lookup(previousSHA256["0.8.0"])
	if !ok {
		t.Fatal("this build does not embed the 0.8.0 BOM")
	}
	return old
}
