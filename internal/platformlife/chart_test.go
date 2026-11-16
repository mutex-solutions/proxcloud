package platformlife

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var updateGolden = flag.Bool("update", false, "rewrite the chart goldens in testdata/")

const platformChart = "../../deploy/charts/proxcloud-platform"

// helmTemplate renders the proxcloud-platform chart in the namespace the BOM gives its three components.
func helmTemplate(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	return exec.Command(helm, append([]string{"template", "platform", platformChart, "-n", "proxcloud-system"}, args...)...).CombinedOutput()
}

func renderExample(t *testing.T, name string) []byte {
	t.Helper()
	out, err := helmTemplate(t, "-f", filepath.Join(platformChart, "examples", "values-"+name+".yaml"))
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", name, err, out)
	}
	return out
}

// yamlObjects decodes a multi-document stream, skipping documents that hold only comments.
func yamlObjects(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for dec := yaml.NewDecoder(bytes.NewReader(data)); ; {
		var o map[string]any
		if err := dec.Decode(&o); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		if o != nil {
			out = append(out, o)
		}
	}
}

func objectKey(o map[string]any) string {
	meta, _ := o["metadata"].(map[string]any)
	ns, _ := meta["namespace"].(string)
	return fmt.Sprintf("%v %v %s/%v", o["apiVersion"], o["kind"], ns, meta["name"])
}

// The chart's output for each BOM component, from its examples/ values file. The golden holds the decoded objects sorted
// by kind and name, so it moves with what ArgoCD applies and not with Helm's formatting, order or comments (the go-test
// job's Helm is not pinned). `go test ./internal/platformlife -run TestPlatformChartGolden -update` rewrites them.
func TestPlatformChartGolden(t *testing.T) {
	for _, name := range []string{"platform", "capi", "capi-tenant"} {
		t.Run(name, func(t *testing.T) {
			objs := yamlObjects(t, renderExample(t, name))
			sort.Slice(objs, func(i, j int) bool { return objectKey(objs[i]) < objectKey(objs[j]) })
			var buf bytes.Buffer
			enc := yaml.NewEncoder(&buf)
			enc.SetIndent(2)
			for _, o := range objs {
				if err := enc.Encode(o); err != nil {
					t.Fatal(err)
				}
			}
			if err := enc.Close(); err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "proxcloud-platform-"+name+".golden.yaml")
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if buf.String() != string(want) {
				t.Errorf("render differs from %s (review, then -update):\n%s", golden, buf.String())
			}
		})
	}
}

// The platform component carries what installer stage 50 applies from deploy/{mgmt,dbaas}-cluster/{rbac,appproject}.yaml,
// object for object: the objects both rbac.yaml files define (Namespace, ServiceAccount, token Secret, argocd Role and
// RoleBinding) once, each provider's ClusterRole and binding, and both tenant AppProjects. Anything else it renders is
// named below.
func TestPlatformChartRendersTheProviderFiles(t *testing.T) {
	want := map[string]map[string]any{}
	for _, f := range []string{"mgmt-cluster/rbac.yaml", "dbaas-cluster/rbac.yaml", "mgmt-cluster/appproject.yaml", "dbaas-cluster/appproject.yaml"} {
		raw, err := os.ReadFile(filepath.Join("../../deploy", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range yamlObjects(t, raw) {
			k := objectKey(o)
			if prev, ok := want[k]; ok && !reflect.DeepEqual(prev, o) {
				t.Fatalf("deploy/%s defines %s unlike the other file, so their union is not one object", f, k)
			}
			want[k] = o
		}
	}
	if len(want) != 11 {
		t.Fatalf("%d objects in the four files, want 11 (5 shared, 2 ClusterRoles and bindings, 2 AppProjects)", len(want))
	}

	got := map[string]map[string]any{}
	for _, o := range yamlObjects(t, renderExample(t, "platform")) {
		k := objectKey(o)
		if _, dup := got[k]; dup {
			t.Errorf("rendered twice: %s", k)
		}
		got[k] = o
	}
	for k, o := range want {
		if !reflect.DeepEqual(got[k], o) {
			t.Errorf("%s: the chart's object differs from its file (change both)\nchart: %v\nfile:  %v", k, got[k], o)
		}
		delete(got, k)
	}
	var extra []string
	for k := range got {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	if w := []string{
		"argoproj.io/v1alpha1 AppProject argocd/default",
		"metallb.io/v1beta1 IPAddressPool metallb-system/services",
		"metallb.io/v1beta1 L2Advertisement metallb-system/services",
		"rbac.authorization.k8s.io/v1 ClusterRole /proxcloud-dbaas-egress",
		"rbac.authorization.k8s.io/v1 ClusterRoleBinding /proxcloud-dbaas-egress",
		"v1 Secret argocd/proxcloud-charts-creds",
	}; !reflect.DeepEqual(extra, w) {
		t.Errorf("objects beyond the four files:\n got %q\nwant %q", extra, w)
	}

	locked := map[string]any{"sourceRepos": []any{}, "destinations": []any{}, "clusterResourceWhitelist": []any{}}
	if spec := got["argoproj.io/v1alpha1 AppProject argocd/default"]["spec"]; !reflect.DeepEqual(spec, locked) {
		t.Errorf("default AppProject not locked: %v", spec)
	}
}

func TestPlatformChartRefuses(t *testing.T) {
	for _, c := range []struct {
		name, want string
		args       []string
	}{
		{"bare defaults", "nothing to render", nil},
		{"the BOM's pending digest", "must be pinned by digest", []string{"--set", "clusterctl.enabled=true"}},
		{"an image by tag", "must be pinned by digest", []string{"--set", "clusterctl.enabled=true",
			"--set", "clusterctl.image=ghcr.io/mutex-solutions/proxcloud-clusterctl:v1.14.4-1"}},
		{"no providers", "clusterctl.providers.core is required", []string{"--set", "clusterctl.enabled=true",
			"--set", "clusterctl.image=ghcr.io/mutex-solutions/proxcloud-clusterctl@sha256:" + strings.Repeat("0", 64)}},
		{"no MetalLB range", "platform.metallb.addresses is required", []string{"--set", "platform.enabled=true"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := helmTemplate(t, c.args...)
			if err == nil || !strings.Contains(string(out), c.want) {
				t.Errorf("want a failure naming %q, got err %v\n%s", c.want, err, out)
			}
		})
	}
}

// The clusterctl Job's args, as the chart renders them, are what the image's entrypoint reads: the entrypoint takes
// each provider as two words (--core NAME:VERSION) and answers anything else with its usage and exit 2. Run here with
// a kubectl that reports an empty cluster and a clusterctl that records its call, so the two cannot drift apart.
func TestClusterctlJobArgsReachTheEntrypoint(t *testing.T) {
	for _, name := range []string{"capi", "capi-tenant"} {
		t.Run(name, func(t *testing.T) {
			var args []string
			for _, o := range yamlObjects(t, renderExample(t, name)) {
				if o["kind"] != "Job" {
					continue
				}
				containers, _ := dig(o, "spec", "template", "spec", "containers").([]any)
				raw, _ := dig(containers[0], "args").([]any)
				for _, a := range raw {
					args = append(args, fmt.Sprint(a))
				}
			}
			if len(args) == 0 {
				t.Fatal("the render has no clusterctl Job args")
			}
			bin, log := t.TempDir(), filepath.Join(t.TempDir(), "calls")
			for file, body := range map[string]string{"kubectl": "#!/bin/sh\nexit 0\n", "clusterctl": "#!/bin/sh\necho \"$*\" >> \"$FAKE_LOG\"\n"} {
				if err := os.WriteFile(filepath.Join(bin, file), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("/bin/sh", append([]string{"../../deploy/clusterctl/clusterctl-init.sh"}, args...)...)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_LOG=" + log, "CLUSTERCTL_INIT_CONFIG=../../deploy/clusterctl/clusterctl.yaml"}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("entrypoint with the chart's args %q: %v\n%s", args, err, out)
			}
			call, _ := os.ReadFile(log)
			for _, a := range args {
				if !strings.Contains(string(call), " "+a+" ") {
					t.Errorf("clusterctl init was not given %q: %q", a, call)
				}
			}
		})
	}
}
