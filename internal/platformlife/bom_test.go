package platformlife

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mutex-solutions/proxcloud/deploy"
)

func embedded(t *testing.T) *BOM {
	t.Helper()
	b, err := Parse(deploy.PlatformBOM)
	if err != nil {
		t.Fatal(err)
	}
	return b
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
		{"a release keeps no pending digest and no 0.0.0+ version", func(_ *testing.T, b *BOM) { b.Release = "0.7.0" },
			[]string{"tenantCharts.kubernetes: digest pending is allowed only in a -dev release", "images.clusterctl: digest pending",
				"workload/kamaji.version: 0.0.0+latest is allowed only in a -dev release"}},
		{"a release with everything pinned", func(_ *testing.T, b *BOM) {
			b.Release = "0.7.0"
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
		{"pending on a third-party chart", func(t *testing.T, b *BOM) { comp(t, b, "workload", "cilium").Chart.Digest = "pending" },
			[]string{"workload/cilium: digest pending is allowed only for our own packages"}},
		{"pending on a third-party image", func(_ *testing.T, b *BOM) {
			b.Images.KubeVip = strings.Split(b.Images.KubeVip, "@")[0] + "@pending"
		}, []string{"images.kubeVip: digest pending is allowed only for our own packages"}},
		{"a chart without digest", func(t *testing.T, b *BOM) { comp(t, b, "workload", "metallb").Chart.Digest = "" }, []string{"workload/metallb: digest \"\""}},
		{"a short digest", func(t *testing.T, b *BOM) { comp(t, b, "workload", "metallb").Chart.Digest = "sha256:96667a81" }, []string{"not sha256:<64 hex>"}},
		{"an image without digest", func(_ *testing.T, b *BOM) { b.Images.KubeVip = "ghcr.io/kube-vip/kube-vip:v0.8.9" }, []string{"images.kubeVip: digest \"\""}},
		{"an image without tag", func(_ *testing.T, b *BOM) { b.Images.KubeVip = "ghcr.io/kube-vip/kube-vip@" + sha }, []string{"not <repo>:<tag>@<digest>"}},
		{"an image at latest", func(_ *testing.T, b *BOM) { b.Images.KubeVip = "ghcr.io/kube-vip/kube-vip:latest@" + sha }, []string{"images.kubeVip tag"}},
		{"a pending VM image", func(_ *testing.T, b *BOM) { b.Seed.Image.Digest = "pending" }, []string{"seed.image: digest"}},
		{"a node image of another version", func(_ *testing.T, b *BOM) { b.Kubernetes.Version = "1.36.6" }, []string{"is not a 1.36.6 node image"}},
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
		{"a release with v", func(_ *testing.T, b *BOM) { b.Release = "v0.7.0-dev" }, []string{"no v prefix"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := embedded(t)
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
// component both name. barman_plugin is not compared: backups are not in the BOM until M2.
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
	kamajiTag := fmt.Sprint(dig(comp(t, b, ClusterWorkload, "kamaji").Values, "image", "tag"))

	for _, p := range []struct{ key, field, bom string }{
		{"k3s", "seed.k3s.version", b.Seed.K3s.Version},
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
		{"kamaji_image", "workload kamaji values image.tag", kamajiTag},
	} {
		if got := fmt.Sprint(vf.V[p.key]); got != p.bom {
			t.Errorf("versions.yml %s = %s, BOM %s = %q", p.key, got, p.field, p.bom)
		}
	}
	if b.Kubernetes.Version != df.D.Services.K8sVersion {
		t.Errorf("pc_defaults.services.k8s_version = %s, BOM kubernetes.version = %s", df.D.Services.K8sVersion, b.Kubernetes.Version)
	}
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
