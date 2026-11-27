package sandbox

import (
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The function objects of the sandbox chart (functions.enabled, chart 0.2.0): what renders with the value off, and
// with it on the role, the proxy's function mode and the egress policy. The admission policies themselves are run in
// test/policy.

// chart010 is the chart's render at tag proxcloud-sandbox-v0.1.0 (sha256 of testdata/chart-<mode>.golden.yaml as that
// tag has it), and the proxy image line of that render.
var chart010 = map[string]string{
	"shared":   "12390ffa014745b2d3f8d31531b700fafe791eb1693919a63c7e1bd170b85003",
	"separate": "73e687754a7730d515ac115535defabd0dbb97c529a979b1733a3a68a30ec93c",
}

const proxyImage010 = "image: ghcr.io/mutex-solutions/proxcloud-sandbox-proxy:v0.11.0-rc.1@sha256:afc0e6e53262ce517d341c428093f7e81106272dda9af0dda6297498bca0e37e"

var proxyImageLine = regexp.MustCompile(`image: ghcr\.io/mutex-solutions/proxcloud-sandbox-proxy:\S+`)

// With functions off the chart renders what 0.1.0 rendered, but for the proxy image: a platform whose BOM moves the
// sandbox chart to this version rolls sandbox-proxy and nothing else. In particular node-setup's DaemonSet is the
// same bytes, so no sandbox node restarts containerd, which would end every sandbox on it. A change that moves this
// render moves every platform with sandboxes: the goldens show what, and this hash is then replaced on purpose.
func TestSandboxChartWithoutFunctionsIsChart010(t *testing.T) {
	for mode, want := range chart010 {
		out := renderSandboxChart(t, mode)
		if n := len(proxyImageLine.FindAllString(out, -1)); n != 1 {
			t.Fatalf("%s: %d proxy image lines, want one", mode, n)
		}
		as010 := proxyImageLine.ReplaceAllString(elideVendored(out), proxyImage010)
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(as010))); got != want {
			t.Errorf("%s: with functions off the chart no longer renders what 0.1.0 rendered but for the proxy image (sha256 %s, want %s); `git diff proxcloud-sandbox-v0.1.0 -- internal/cloud/sandbox/testdata/chart-%s.golden.yaml` shows what moved", mode, got, want, mode)
		}
		// Off is off whatever else the block holds: the settings of a platform that switched functions off again.
		leftover := renderSandboxChart(t, mode, "--set", "functions.enabled=false", "--set", "functions.wakeURL=https://api.example.org/api/v1/internal/functions",
			"--set-string", "functions.wakeTokenVersion=7", "--set", "functions.portalCA=PEM")
		if leftover != out {
			t.Errorf("%s: functions.enabled=false with the other function values set renders something of them", mode)
		}
	}
}

// bySource are the documents of a render that one template file made, in order.
func bySource(out, file string) []string {
	var docs []string
	for _, d := range strings.Split(out, "\n---\n") {
		if strings.HasPrefix(d, "# Source: proxcloud-sandbox/templates/"+file+"\n") {
			docs = append(docs, d)
		}
	}
	return docs
}

// Switching functions on touches the system side only. Everything that runs on a sandbox node (node-setup with its
// script, telemetry, the prepull) and agent-sandbox render the same bytes, so the apply that enables functions
// restarts no containerd and no sandbox; and of the runtime file (RuntimeClass, Cilium settings, the guardrail) the
// one difference is the added egress policy.
func TestSandboxChartFunctionsLeaveTheNodeSideAlone(t *testing.T) {
	for _, mode := range []string{"shared", "separate"} {
		off, on := renderSandboxChart(t, mode), renderSandboxChart(t, mode+"-functions")
		for _, file := range []string{"node.yaml", "agent-sandbox.yaml"} {
			a, b := bySource(off, file), bySource(on, file)
			if len(a) == 0 || !reflect.DeepEqual(a, b) {
				t.Errorf("%s: %s renders differently with functions on (%d and %d documents)", mode, file, len(a), len(b))
			}
		}
		find(t, yamlDocs(t, strings.Join(bySource(on, "node.yaml"), "\n---\n")), "DaemonSet", "node-setup") // it is among them
		a, b := bySource(off, "runtime.yaml"), bySource(on, "runtime.yaml")
		var added []string
		for _, d := range b {
			if !strings.Contains("\n---\n"+strings.Join(a, "\n---\n")+"\n---\n", "\n---\n"+d+"\n---\n") {
				added = append(added, d)
			}
		}
		if len(b) != len(a)+1 || len(added) != 1 || !strings.Contains(added[0], "name: function-tenant-egress-internet") {
			t.Errorf("%s: functions change the runtime file by more than the one egress policy: %v", mode, added)
		}
	}
}

func rules(t *testing.T, docs []map[string]any, role string) []string {
	t.Helper()
	var out []string
	for _, r := range find(t, docs, "ClusterRole", role)["rules"].([]any) {
		m := r.(map[string]any)
		line := fmt.Sprintf("%v %v %v", m["apiGroups"], m["resources"], m["verbs"])
		if n := m["resourceNames"]; n != nil {
			line += fmt.Sprintf(" %v", n)
		}
		out = append(out, line)
	}
	return out
}

// functionRoleRules is what the portal's identity may do in a function namespace: the calls internal/cloud/function
// makes (roleVerbs in its service_test.go is the same list) and no more. No watch, no read of a scale, no read and
// no delete of a Secret, no exec, no Sandbox kind, no network policy.
var functionRoleRules = []string{
	"[apps] [deployments] [get list create patch delete]",
	"[apps] [deployments/scale] [patch]",
	"[] [secrets] [create patch]",
	"[] [pods] [list]",
	"[] [pods/log] [get]",
	"[] [resourcequotas] [create patch]",
}

// The exact verb list, in both modes. Shared: a ClusterRole of its own, which the identity may bind beside the
// sandbox one and holds nothing of cluster-wide. Separate: the same rules join its cluster-wide list, and nothing
// else does.
func TestSandboxChartFunctionRole(t *testing.T) {
	shared := yamlDocs(t, renderSandboxChart(t, "shared-functions"))
	if got := rules(t, shared, FunctionRole); !reflect.DeepEqual(got, functionRoleRules) {
		t.Errorf("shared: %s grants\n %v\nwant\n %v", FunctionRole, got, functionRoleRules)
	}
	off := yamlDocs(t, renderSandboxChart(t, "shared"))
	want := append([]string{}, rules(t, off, "proxcloud-sandbox")...)
	for i, r := range want {
		want[i] = strings.Replace(r, "["+SharedRole+"]", "["+SharedRole+" "+FunctionRole+"]", 1)
	}
	if got := rules(t, shared, "proxcloud-sandbox"); !reflect.DeepEqual(got, want) || reflect.DeepEqual(got, rules(t, off, "proxcloud-sandbox")) {
		t.Errorf("shared: the identity's cluster-wide role must change by the second role it may bind, and by nothing else:\n %v\nwant\n %v", got, want)
	}
	if got, want := rules(t, shared, SharedRole), rules(t, off, SharedRole); !reflect.DeepEqual(got, want) {
		t.Errorf("shared: functions changed the sandbox role: %v", got)
	}
	for _, d := range off {
		if d["metadata"].(map[string]any)["name"] == FunctionRole {
			t.Errorf("functions off: a %v %s renders", d["kind"], FunctionRole)
		}
	}

	sepOff, sepOn := rules(t, yamlDocs(t, renderSandboxChart(t, "separate")), "proxcloud-sandbox"), rules(t, yamlDocs(t, renderSandboxChart(t, "separate-functions")), "proxcloud-sandbox")
	nodes := sepOff[len(sepOff)-1]
	if want := append(append(append([]string{}, sepOff[:len(sepOff)-1]...), functionRoleRules...), nodes); !reflect.DeepEqual(sepOn, want) {
		t.Errorf("separate: the identity's cluster-wide rules with functions\n %v\nwant those without and the function rules\n %v", sepOn, want)
	}
}

func proxyPod(t *testing.T, docs []map[string]any) (annotations map[string]any, spec map[string]any) {
	t.Helper()
	tpl := find(t, docs, "Deployment", "sandbox-proxy")["spec"].(map[string]any)["template"].(map[string]any)
	annotations, _ = tpl["metadata"].(map[string]any)["annotations"].(map[string]any)
	return annotations, tpl["spec"].(map[string]any)
}

// The proxy's function mode: the two settings it reads, the one new right (a list of Deployments), and the portal's
// CA as a file its roots are read from. FUNCTION_DOMAIN is the sandbox domain: one address and one certificate serve
// both. Without a wake URL the proxy would serve no function and say so only in its log, so the render is refused.
func TestSandboxChartProxyFunctionMode(t *testing.T) {
	for _, mode := range []string{"shared-functions", "separate-functions"} {
		docs := yamlDocs(t, renderSandboxChart(t, mode))
		cm := find(t, docs, "ConfigMap", "sandbox-proxy-config")["data"].(map[string]any)
		if cm["FUNCTION_DOMAIN"] != cm["SANDBOX_DOMAIN"] || cm["FUNCTION_DOMAIN"] != "sbx.example.org" {
			t.Errorf("%s: FUNCTION_DOMAIN %v, want the sandbox domain %v", mode, cm["FUNCTION_DOMAIN"], cm["SANDBOX_DOMAIN"])
		}
		if u, _ := cm["PROXCLOUD_FUNCTION_WAKE_URL"].(string); !strings.HasSuffix(u, "/api/v1/internal/functions") {
			t.Errorf("%s: PROXCLOUD_FUNCTION_WAKE_URL %q", mode, u)
		}
		if got, want := rules(t, docs, "sandbox-proxy"), []string{"[] [pods] [list]", "[apps] [deployments] [list]"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the proxy may %v, want %v: it only reads, and never a Secret", mode, got, want)
		}
		_, spec := proxyPod(t, docs)
		c := spec["containers"].([]any)[0].(map[string]any)
		// The token comes from the Secret the run writes, whole and optional, as before: no key of it is named here.
		if got, want := fmt.Sprint(c["envFrom"]), "[map[configMapRef:map[name:sandbox-proxy-config]] map[secretRef:map[name:sandbox-proxy-wake optional:true]]]"; got != want {
			t.Errorf("%s: envFrom %s", mode, got)
		}
	}
	if _, err := helmSandboxChart(t, "shared-functions", "--set", "functions.wakeURL="); err == nil || !strings.Contains(err.Error(), "functions.wakeURL is required") {
		t.Errorf("functions on without a wake URL must not render: %v", err)
	}

	// The CA: only when one is given (the separate example has one, the shared example's portal is publicly signed).
	docs := yamlDocs(t, renderSandboxChart(t, "separate-functions"))
	var values struct {
		Functions struct {
			PortalCA string `yaml:"portalCA"`
		} `yaml:"functions"`
	}
	b, err := os.ReadFile(sandboxChart + "/examples/values-separate-functions.yaml")
	if err != nil || yaml.Unmarshal(b, &values) != nil || !strings.HasPrefix(values.Functions.PortalCA, "-----BEGIN CERTIFICATE-----\n") {
		t.Fatalf("the separate example carries no CA: %v", err)
	}
	if got := find(t, docs, "ConfigMap", "sandbox-proxy-portal-ca")["data"].(map[string]any)["ca.crt"]; got != values.Functions.PortalCA {
		t.Errorf("the CA the proxy reads is not the one given:\n%v", got)
	}
	_, spec := proxyPod(t, docs)
	c := spec["containers"].([]any)[0].(map[string]any)
	if got := fmt.Sprint(c["env"]); !strings.Contains(got, "map[name:SSL_CERT_FILE value:/etc/sandbox-proxy/portal-ca/ca.crt]") {
		t.Errorf("env %s: no SSL_CERT_FILE", got)
	}
	if got := fmt.Sprint(c["volumeMounts"]); !strings.Contains(got, "map[mountPath:/etc/sandbox-proxy/portal-ca name:portal-ca readOnly:true]") {
		t.Errorf("volumeMounts %s", got)
	}
	if got := fmt.Sprint(spec["volumes"]); !strings.Contains(got, "map[configMap:map[name:sandbox-proxy-portal-ca] name:portal-ca]") {
		t.Errorf("volumes %s", got)
	}
	shared := renderSandboxChart(t, "shared-functions")
	if strings.Contains(shared, "SSL_CERT_FILE") || strings.Contains(shared, "portal-ca") {
		t.Error("no CA given: the proxy must keep the image's roots, nothing mounted")
	}
}

// The wake token is written outside the chart and read by the proxy once, when it starts: so the run names each token
// by a version, and that version is on the proxy's pod template, where a new one replaces the pods. It changes when
// and only when the version does. A changed portal URL, CA or domain replaces the pods too (the same once-read
// environment), through an annotation of its own. The token itself has no way into a render: the block has no key
// for it, and the chart makes no Secret but the two service account tokens.
func TestSandboxChartWakeTokenVersionRollsTheProxy(t *testing.T) {
	const versionKey, configKey = "proxcloud.io/function-wake-token-version", "checksum/function-config"
	base := renderSandboxChart(t, "shared-functions")
	next := renderSandboxChart(t, "shared-functions", "--set-string", "functions.wakeTokenVersion=2")
	a, _ := proxyPod(t, yamlDocs(t, base))
	b, _ := proxyPod(t, yamlDocs(t, next))
	if a[versionKey] != "1" || b[versionKey] != "2" {
		t.Fatalf("the version is not on the proxy's pod template: %v, %v", a, b)
	}
	if want := strings.Replace(base, versionKey+`: "1"`, versionKey+`: "2"`, 1); next != want {
		t.Error("a new token version must change the proxy's pod template annotation and nothing else")
	}
	for name, set := range map[string]string{
		"the portal URL": "functions.wakeURL=https://portal.example.org/api/v1/internal/functions",
		"the portal CA":  "functions.portalCA=PEM",
		"the domain":     "proxy.domain=other.example.org",
	} {
		c, _ := proxyPod(t, yamlDocs(t, renderSandboxChart(t, "shared-functions", "--set", set)))
		if c[versionKey] != a[versionKey] {
			t.Errorf("%s changed the token version annotation: %v", name, c[versionKey])
		}
		if c[configKey] == a[configKey] || c[configKey] == nil {
			t.Errorf("%s changed and the proxy's pods would keep the old setting: %s is %v", name, configKey, c[configKey])
		}
	}
	if a[configKey] != b[configKey] {
		t.Error("the settings digest must not move with the token version")
	}
	if off, _ := proxyPod(t, yamlDocs(t, renderSandboxChart(t, "shared"))); off != nil {
		t.Errorf("functions off: the proxy's pod template carries annotations %v", off)
	}

	var values struct {
		Functions map[string]any `yaml:"functions"`
	}
	raw, err := os.ReadFile(sandboxChart + "/values.yaml")
	if err != nil || yaml.Unmarshal(raw, &values) != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range values.Functions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"enabled", "portalCA", "wakeTokenVersion", "wakeURL"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("the functions block has keys %v, want %v: a token is never a value (the chart sits in an Application)", keys, want)
	}
	for _, mode := range []string{"shared-functions", "separate-functions"} {
		out := renderSandboxChart(t, mode)
		for _, d := range yamlDocs(t, out) {
			if d["kind"] == "Secret" && (d["type"] != "kubernetes.io/service-account-token" || d["data"] != nil || d["stringData"] != nil) {
				t.Errorf("%s: the chart renders Secret %v", mode, d["metadata"])
			}
		}
		if strings.Contains(out, "WAKE_TOKEN") {
			t.Errorf("%s: a render names a wake token variable; the proxy takes it from Secret sandbox-proxy-wake as a whole", mode)
		}
	}
}

// Functions reach the internet through one cluster-wide policy, which selects a namespace by the tenant label AND the
// function label: where it allows, the guardrail's denies hold (the private ranges, the platform prefix, TCP 25), and
// a sandbox namespace is not opened by it. It allows a CIDR and nothing in the cluster.
func TestSandboxChartFunctionEgress(t *testing.T) {
	for _, mode := range []string{"shared-functions", "separate-functions"} {
		docs := yamlDocs(t, renderSandboxChart(t, mode))
		got := find(t, docs, "CiliumClusterwideNetworkPolicy", "function-tenant-egress-internet")["spec"]
		want := map[string]any{
			"endpointSelector": map[string]any{"matchLabels": map[string]any{
				"k8s:io.cilium.k8s.namespace.labels." + LabelTenant:         "true",
				"k8s:io.cilium.k8s.namespace.labels." + LabelFunctionTenant: "true",
			}},
			"egress": []any{map[string]any{"toCIDRSet": []any{map[string]any{"cidr": "0.0.0.0/0"}}}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: function-tenant-egress-internet\n got %v\nwant %v", mode, got, want)
		}
		deny := fmt.Sprint(find(t, docs, "CiliumClusterwideNetworkPolicy", "sandbox-tenant-egress")["spec"].(map[string]any)["egressDeny"])
		for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "192.0.2.0/24", "port:25"} {
			if !strings.Contains(deny, cidr) {
				t.Errorf("%s: the guardrail no longer denies %s to a tenant pod: %s", mode, cidr, deny)
			}
		}
	}
}
