package sandbox

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// sharedRBAC is the API server as the portal's identity meets it on a shared cluster, after the sandbox chart's
// identities (templates/identities.yaml, mode shared): cluster-wide it may read and write namespaces and write
// RoleBindings; every other right is the namespaced ClusterRole SharedRole, which holds only where a RoleBinding of
// it exists. The two admission policies are here too: a RoleBinding only in an sb-* namespace labelled as a tenant's,
// only of that role, only to the identity itself. TestSharedIdentityIsTheCharts ties these rules to the chart.
type sharedRBAC struct {
	*sampleAPI
	refused []string
}

func (a *sharedRBAC) deny(what string) error {
	a.refused = append(a.refused, what)
	return &kamajik8s.APIError{Status: 403, Message: what + " is forbidden"}
}

func (a *sharedRBAC) bound(ns string) bool {
	rb := a.crs[ns+"/"+SharedAccount]
	return rb != nil && rb["kind"] == "RoleBinding" && digStr(rb, "roleRef", "name") == SharedRole
}

func (a *sharedRBAC) ListNamespaces(_ context.Context, sel string) ([]map[string]any, error) {
	var out []map[string]any
	for name, labels := range a.namespaces {
		ok := true
		for _, kv := range strings.Split(sel, ",") {
			k, v, _ := strings.Cut(kv, "=")
			ok = ok && labels[k] == v
		}
		if ok {
			out = append(out, map[string]any{"metadata": map[string]any{"name": name}})
		}
	}
	return out, nil
}

func (a *sharedRBAC) ListCRsAllNamespaces(context.Context, string, string, string, string) ([]map[string]any, error) {
	return nil, a.deny("list sandboxes at the cluster scope")
}

func (a *sharedRBAC) ListPodsAllNamespaces(context.Context, string) ([]map[string]any, error) {
	return nil, a.deny("list pods at the cluster scope")
}

func (a *sharedRBAC) ListCRs(ctx context.Context, g, v, plural, ns, sel string) ([]map[string]any, error) {
	if !a.bound(ns) {
		return nil, a.deny("list " + plural + " in " + ns)
	}
	return a.fakeAPI.ListCRs(ctx, g, v, plural, ns, sel)
}

func (a *sharedRBAC) ListPods(ctx context.Context, ns, sel string) ([]map[string]any, error) {
	if !a.bound(ns) {
		return nil, a.deny("list pods in " + ns)
	}
	return a.fakeAPI.ListPods(ctx, ns, sel)
}

func (a *sharedRBAC) ApplyResourceQuota(ctx context.Context, rq map[string]any) error {
	if ns := digStr(rq, "metadata", "namespace"); !a.bound(ns) {
		return a.deny("patch resourcequotas in " + ns)
	}
	return a.fakeAPI.ApplyResourceQuota(ctx, rq)
}

func (a *sharedRBAC) ApplyCR(ctx context.Context, g, v, plural string, obj map[string]any) error {
	ns := digStr(obj, "metadata", "namespace")
	if plural != "rolebindings" {
		if !a.bound(ns) {
			return a.deny("patch " + plural + " in " + ns)
		}
		return a.fakeAPI.ApplyCR(ctx, g, v, plural, obj)
	}
	subjects, _ := obj["subjects"].([]any)
	self := len(subjects) == 1 && reflect.DeepEqual(subjects[0], map[string]any{"kind": "ServiceAccount", "name": SharedAccount, "namespace": SharedAccountNS})
	if !strings.HasPrefix(ns, "sb-") || a.namespaces[ns][LabelTenant] != "true" || digStr(obj, "roleRef", "kind") != "ClusterRole" || digStr(obj, "roleRef", "name") != SharedRole || !self {
		return a.deny("the admission policy: rolebinding in " + ns)
	}
	return a.fakeAPI.ApplyCR(ctx, g, v, plural, obj)
}

func newSharedAPI() *sharedRBAC {
	return &sharedRBAC{sampleAPI: &sampleAPI{fakeAPI: newFake()}}
}

// The names the portal binds with are the ones the chart renders, and the chart's cluster-wide role of the identity
// holds nothing namespaced: read from the golden render, which TestSandboxChartGolden keeps equal to helm's output.
func TestSharedIdentityIsTheCharts(t *testing.T) {
	raw, err := os.ReadFile("testdata/chart-shared.golden.yaml")
	if err != nil {
		t.Fatal(err)
	}
	docs := yamlDocs(t, string(raw))
	if ns := find(t, docs, "ServiceAccount", SharedAccount)["metadata"].(map[string]any)["namespace"]; ns != SharedAccountNS {
		t.Fatalf("the identity is in namespace %v, the portal binds it in %s", ns, SharedAccountNS)
	}
	var wide []string
	for _, r := range find(t, docs, "ClusterRole", SharedAccount)["rules"].([]any) {
		for _, res := range r.(map[string]any)["resources"].([]any) {
			wide = append(wide, res.(string))
		}
	}
	slices.Sort(wide)
	if !slices.Equal(wide, []string{"clusterroles", "namespaces", "nodes", "rolebindings"}) {
		t.Fatalf("cluster-wide the identity holds %v: sharedRBAC and the per-namespace lists assume namespaces, rolebindings, the bind and nodes alone", wide)
	}
	var namespaced []string
	for _, r := range find(t, docs, "ClusterRole", SharedRole)["rules"].([]any) {
		for _, res := range r.(map[string]any)["resources"].([]any) {
			namespaced = append(namespaced, res.(string))
		}
	}
	for _, need := range []string{"resourcequotas", "pods", "pods/exec", "pods/log", "sandboxes", "ciliumnetworkpolicies"} {
		if !slices.Contains(namespaced, need) {
			t.Errorf("%s holds no %s: %v", SharedRole, need, namespaced)
		}
	}
	sched := find(t, docs, "RuntimeClass", "gvisor")["scheduling"].(map[string]any)
	if !reflect.DeepEqual(sched["nodeSelector"], map[string]any{LabelNodeGroup: NodeGroupSandbox}) {
		t.Fatalf("the RuntimeClass selects %v; the gates read the sandbox machines by %s=%s", sched["nodeSelector"], LabelNodeGroup, NodeGroupSandbox)
	}
	tol := sched["tolerations"].([]any)
	if len(tol) != 1 || tol[0].(map[string]any)["key"] != TaintSandbox {
		t.Fatalf("the RuntimeClass tolerates %v, not %s alone", tol, TaintSandbox)
	}
}

func TestSharedClusterBindsEachNamespaceAndListsThroughThem(t *testing.T) {
	ctx := context.Background()
	api := newSharedAPI()
	cfg := testConfig()
	cfg.Shared = true
	s := NewWithAPI(api, cfg, "svc")
	for _, p := range []string{"p1", "p2"} {
		if err := s.EnsureProjectNamespace(ctx, p, nil); err != nil {
			t.Fatalf("project %s: %v", p, err)
		}
		if !api.bound("sb-"+p) || api.quotas["sb-"+p] == nil {
			t.Fatalf("sb-%s: no RoleBinding of %s, or no quota", p, SharedRole)
		}
	}
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	for _, p := range []string{"p1", "p2"} {
		cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000000000000" + p[1:] + "0", ProjectID: p, Template: "python", CPU: 1, MemoryGiB: 1}, time.Now())
		ann := cr["metadata"].(map[string]any)["annotations"].(map[string]any)
		ann[AnnotationAutoPause], ann[AnnotationEndAt] = "true", past
		if err := api.ApplyCR(ctx, CRGroup, CRVersion, CRPlural, cr); err != nil {
			t.Fatal(err)
		}
	}
	// Another provider's project on the same cluster, and a namespace that only looks like one: neither is this service's.
	api.namespaces["sb-other"] = map[string]string{LabelTenant: "true", LabelService: "svc-2"}
	api.namespaces["kube-system"] = map[string]string{}
	due, err := s.DueForAutoPause(ctx)
	if err != nil || len(due) != 2 {
		t.Fatalf("due = %v %v: both projects, read namespace by namespace", due, err)
	}
	if len(api.refused) != 0 {
		t.Fatalf("the identity was refused: %v", api.refused)
	}
	// The same calls as the installer's cluster-wide identity makes them are refused here: the mode is what holds.
	cfg.Shared = false
	plain := NewWithAPI(api, cfg, "svc")
	if _, err := plain.DueForAutoPause(ctx); err == nil {
		t.Fatal("a cluster-wide list passed on a shared cluster")
	}
	if err := plain.EnsureProjectNamespace(ctx, "p3", nil); err == nil {
		t.Fatal("a quota was written in a namespace without the RoleBinding")
	}
}

// The network guard gate on a shared cluster: the platform's machines keep Cilium's own CNI config and are not asked
// for; the sandbox machines are, by the label a kubelet cannot set.
func TestNetworkGuardGateOnASharedCluster(t *testing.T) {
	platformCM := map[string]any{"custom-cni-conf": "false", "write-cni-conf-when-ready": "/host/etc/cni/net.d/05-cilium.conflist"}
	cnc := map[string]any{"metadata": map[string]any{"name": "sandbox-nodes", "namespace": "kube-system"}, "spec": map[string]any{
		"nodeSelector": map[string]any{"matchLabels": map[string]any{"proxcloud.io/node-group": "sandbox"}},
		"defaults":     map[string]any{"custom-cni-conf": "true", "write-cni-conf-when-ready": ""}}}
	node := func(name, group, kubeletGroup string) map[string]any {
		labels := map[string]any{"proxcloud.io/node-group": kubeletGroup}
		if group != "" {
			labels[LabelNodeGroup] = group
		}
		return map[string]any{"metadata": map[string]any{"name": name, "labels": labels}}
	}
	heartbeat := func(n string, age time.Duration) map[string]any {
		return map[string]any{"data": map[string]any{"node": n, "at": time.Now().Add(-age).UTC().Format(time.RFC3339), "version": "1"}}
	}
	platform := []map[string]any{node("cp-a", "", "control-plane"), node("system-a", "system", "system"), node("kamaji-a", "kamaji", "kamaji")}
	for _, c := range []struct {
		name   string
		shared bool
		nodes  []map[string]any
		beats  []map[string]any
		want   string // "" = passes
	}{
		{"platform machines and one guarded sandbox machine", true, append(slices.Clone(platform), node("sbx-a", "sandbox", "sandbox")), []map[string]any{heartbeat("sbx-a", 0)}, ""},
		{"the same cluster read as a sandbox cluster of its own", false, append(slices.Clone(platform), node("sbx-a", "sandbox", "sandbox")), []map[string]any{heartbeat("sbx-a", 0)}, "still owns the CNI config on node"},
		{"no sandbox machine", true, platform, nil, "no network-guarded sandbox nodes"},
		{"a second sandbox machine without the guard", true, append(slices.Clone(platform), node("sbx-a", "sandbox", "sandbox"), node("sbx-b", "sandbox", "sandbox")), []map[string]any{heartbeat("sbx-a", 0)}, "missing or stale on node sbx-b"},
		{"a stale heartbeat", true, append(slices.Clone(platform), node("sbx-a", "sandbox", "sandbox")), []map[string]any{heartbeat("sbx-a", 5*time.Minute)}, "missing or stale on node sbx-a"},
		{"a sandbox machine Cilium still writes the config of", true, append(slices.Clone(platform), node("sbx-a", "sandbox", "system")), []map[string]any{heartbeat("sbx-a", 0)}, "still owns the CNI config on node sbx-a"},
		{"a platform machine that claims the kubelet label is no sandbox machine", true, []map[string]any{node("system-a", "system", "sandbox")}, []map[string]any{heartbeat("system-a", 0)}, "no network-guarded sandbox nodes"},
	} {
		f := newFake()
		f.core["configmaps/kube-system/cilium-config"] = map[string]any{"data": platformCM}
		f.crs["kube-system/sandbox-nodes"] = cnc
		f.nodes = c.nodes
		cfg := testConfig()
		cfg.Shared = c.shared
		err := NewWithAPI(&sampleAPI{fakeAPI: f, cms: c.beats}, cfg, "svc").CheckNetworkGuard(context.Background())
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err=%v, want %q", c.name, err, c.want)
		}
	}
}
