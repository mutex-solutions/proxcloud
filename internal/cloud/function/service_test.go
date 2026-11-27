package function

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// fakeAPI is an in-memory cluster: namespaces, quotas, Deployments and RoleBindings by "ns/name", pods, Secrets.
// It keeps every call it gets as the verbs the API server would ask of the caller (calls), which is how the tests
// hold the service to the rights of its role.
type fakeAPI struct {
	namespaces map[string]map[string]string
	quotas     map[string]map[string]any
	deps       map[string]map[string]any // "ns/name"
	bindings   map[string]map[string]any // RoleBindings, "ns/name"
	pods       []map[string]any
	core       map[string]map[string]any // applied core objects, "plural/ns/name"
	logErr     map[string]error          // pod name -> PodLogs error
	selectors  []string
	ops        []string // writes of Deployments and core objects, in order
	delErr     error    // what DeleteCR fails with
	calls      []string // "<verb> <resource>" in a namespace, "<verb> <resource> (cluster)" at the cluster scope
}

func newFake() *fakeAPI {
	return &fakeAPI{
		namespaces: map[string]map[string]string{}, quotas: map[string]map[string]any{}, deps: map[string]map[string]any{},
		bindings: map[string]map[string]any{}, core: map[string]map[string]any{}, logErr: map[string]error{},
	}
}

// did records a call. A server-side apply is a patch, and a create when the object is not there yet.
func (f *fakeAPI) did(verb, resource string) {
	if verb == "apply" {
		f.calls = append(f.calls, "create "+resource, "patch "+resource)
		return
	}
	f.calls = append(f.calls, verb+" "+resource)
}

func (f *fakeAPI) EnsureNamespace(_ context.Context, name string, labels, annotations map[string]string) error {
	if annotations != nil {
		return errors.New("a function namespace has no annotations")
	}
	f.did("apply", "namespaces (cluster)")
	f.namespaces[name] = labels
	return nil
}
func (f *fakeAPI) GetNamespace(_ context.Context, name string) (map[string]any, error) {
	f.did("get", "namespaces (cluster)")
	l, ok := f.namespaces[name]
	if !ok {
		return nil, nil
	}
	lab := map[string]any{}
	for k, v := range l {
		lab[k] = v
	}
	return map[string]any{"metadata": map[string]any{"name": name, "labels": lab}}, nil
}
func (f *fakeAPI) DeleteNamespace(_ context.Context, name string) error {
	f.did("delete", "namespaces (cluster)")
	delete(f.namespaces, name)
	return nil
}
func (f *fakeAPI) ListNamespaces(_ context.Context, sel string) ([]map[string]any, error) {
	f.did("list", "namespaces (cluster)")
	var out []map[string]any
	for name, labels := range f.namespaces {
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
func (f *fakeAPI) ApplyResourceQuota(_ context.Context, rq map[string]any) error {
	f.did("apply", "resourcequotas")
	f.quotas[digStr(rq, "metadata", "namespace")] = rq
	return nil
}

// ApplyCR behaves like a server-side apply of a Deployment: a new object gets the API default of
// one replica, an existing one keeps its replica count and status (the manifest owns neither).
func (f *fakeAPI) ApplyCR(_ context.Context, group, version, plural string, obj map[string]any) error {
	key := digStr(obj, "metadata", "namespace") + "/" + digStr(obj, "metadata", "name")
	if group+"/"+version+"/"+plural == "rbac.authorization.k8s.io/v1/rolebindings" {
		f.did("apply", "rolebindings (cluster)")
		f.bindings[key] = obj
		return nil
	}
	if group+"/"+version+"/"+plural != "apps/v1/deployments" {
		return errors.New("unexpected resource " + group + "/" + version + "/" + plural)
	}
	f.did("apply", "deployments")
	f.ops = append(f.ops, "apply deployment")
	old := f.deps[key]
	obj["metadata"].(map[string]any)["uid"] = "uid-" + digStr(obj, "metadata", "name")
	if old != nil {
		obj["status"] = old["status"]
	}
	if spec := obj["spec"].(map[string]any); spec["replicas"] == nil { // a manifest that sets it wins
		spec["replicas"] = 1
		if old != nil {
			spec["replicas"] = dig(old, "spec", "replicas")
		}
	}
	f.deps[key] = obj
	return nil
}
func (f *fakeAPI) GetCR(_ context.Context, _, _, plural, ns, name string) (map[string]any, error) {
	f.did("get", plural)
	return f.deps[ns+"/"+name], nil
}
func (f *fakeAPI) DeleteCR(_ context.Context, _, _, plural, ns, name string) error {
	f.did("delete", plural)
	if f.delErr != nil {
		return f.delErr
	}
	f.ops = append(f.ops, "delete deployment")
	delete(f.deps, ns+"/"+name)
	return nil
}
func (f *fakeAPI) MergePatchCR(_ context.Context, _, _, plural, ns, name string, patch map[string]any) (map[string]any, error) {
	f.did("patch", plural)
	obj := f.deps[ns+"/"+name]
	mergePatch(obj, patch)
	return obj, nil
}
func mergePatch(obj, patch map[string]any) {
	for k, v := range patch {
		if m, ok := v.(map[string]any); ok {
			cur, _ := obj[k].(map[string]any)
			if cur == nil {
				cur = map[string]any{}
				obj[k] = cur
			}
			mergePatch(cur, m)
		} else if v == nil {
			delete(obj, k)
		} else {
			obj[k] = v
		}
	}
}
func (f *fakeAPI) ListCRs(_ context.Context, _, _, plural, ns, sel string) ([]map[string]any, error) {
	f.did("list", plural)
	f.selectors = append(f.selectors, sel)
	var out []map[string]any
	for k, v := range f.deps {
		if strings.HasPrefix(k, ns+"/") {
			out = append(out, v)
		}
	}
	return out, nil
}
func (f *fakeAPI) ListCRsAllNamespaces(_ context.Context, _, _, plural, sel string) ([]map[string]any, error) {
	f.did("list", plural+" (cluster)")
	f.selectors = append(f.selectors, sel)
	var out []map[string]any
	for _, v := range f.deps {
		out = append(out, v)
	}
	return out, nil
}

// replicas of the test function's Deployment.
func (f *fakeAPI) replicas() int { return intOf(dig(f.deps["fn-p1/"+testID], "spec", "replicas")) }

func (f *fakeAPI) SetScale(_ context.Context, _, _, plural, ns, name string, replicas int) error {
	f.did("patch", plural+"/scale")
	dep := f.deps[ns+"/"+name]
	if dep == nil {
		return &kamajik8s.APIError{Status: http.StatusNotFound}
	}
	dep["spec"].(map[string]any)["replicas"] = replicas
	return nil
}
func (f *fakeAPI) ListPods(_ context.Context, _, sel string) ([]map[string]any, error) {
	f.did("list", "pods")
	f.selectors = append(f.selectors, sel)
	return f.pods, nil
}
func (f *fakeAPI) PodLogs(_ context.Context, _, pod, container string, _ int) (string, error) {
	f.did("get", "pods/log")
	if err := f.logErr[pod]; err != nil {
		return "", err
	}
	return "log of " + pod + "/" + container + "\n", nil
}
func (f *fakeAPI) ApplyCore(_ context.Context, plural string, obj map[string]any) error {
	f.did("apply", plural)
	f.ops = append(f.ops, "apply "+plural)
	f.core[plural+"/"+digStr(obj, "metadata", "namespace")+"/"+digStr(obj, "metadata", "name")] = obj
	return nil
}
func (f *fakeAPI) MergePatchCore(_ context.Context, plural, ns, name string, patch map[string]any) error {
	f.did("patch", plural)
	obj := f.core[plural+"/"+ns+"/"+name]
	if obj == nil {
		return &kamajik8s.APIError{Status: http.StatusNotFound}
	}
	f.ops = append(f.ops, "patch "+plural)
	mergePatch(obj, patch)
	return nil
}

var _ K8sAPI = (*kamajik8s.Client)(nil)

const (
	testID  = "fn-0123456789abcdef"
	testSvc = "svc-1"
)

var testQuota = sandbox.FunctionQuota{Functions: 20, VCPUs: 8, RAMMb: 16384}

func testService() (*Service, *fakeAPI) {
	f := newFake()
	return NewWithAPI(f, testConfig(), testSvc), f
}

func testFunction() Function {
	return Function{ID: testID, ProjectID: "p1", Host: "api-a1b2c3.sbx.example.com"}
}

var testSnap = Snapshot{Image: "nginx:1.27", Port: 8080, CPUMillis: 250, MemoryMi: 256}

// pod builds a function pod: scheduled unless node is "", with one container in the given state
// ("" = no container status yet; "waiting:<reason>", "running", "terminated").
func pod(name, node, state string, ready bool) map[string]any {
	st := map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": map[bool]string{true: "True", false: "False"}[ready]}}}
	if kind, reason, _ := strings.Cut(state, ":"); kind != "" {
		st["containerStatuses"] = []any{map[string]any{"name": "main", "state": map[string]any{kind: map[string]any{"reason": reason}}}}
	}
	return map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"nodeName": node}, "status": st}
}

func TestNew(t *testing.T) {
	if _, err := New(testConfig(), testSvc); err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Fatalf("no kubeconfig: %v", err)
	}
	// A provider that does not offer functions (off, or an invalid block, or no domain to serve them under) still
	// gets a service: what runs there must stay stoppable. Enabled tells the two apart.
	on := sandbox.FunctionsConfig{Enabled: true}
	bad := on
	bad.MaxInstances = sandbox.MaxFunctionInstances + 1
	for _, c := range []struct {
		cfg  sandbox.Config
		want bool
	}{
		{sandbox.Config{}, false},
		{sandbox.Config{Domain: "sbx.example.com"}, false}, // the block is off
		{sandbox.Config{Functions: on}, false},             // no domain
		{sandbox.Config{Functions: on, Domain: "sbx.example.com;x"}, false},
		{sandbox.Config{Functions: bad, Domain: "sbx.example.com"}, false},
		{sandbox.Config{Functions: on, Domain: "sbx.example.com"}, true},
	} {
		s := NewWithAPI(newFake(), c.cfg, testSvc)
		if got := s.Enabled(); got != c.want || s.Domain() != c.cfg.Domain {
			t.Errorf("Enabled(%+v, domain %q) = %v, want %v", c.cfg.Functions, c.cfg.Domain, got, c.want)
		}
	}
}

func TestEnsureAndDeleteNamespace(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	if err := s.EnsureNamespace(ctx, "p1", sandbox.FunctionQuota{Functions: 4, VCPUs: 2, RAMMb: 2048}); err != nil {
		t.Fatal(err)
	}
	// The tenant label every tenant namespace carries puts it under the pod policy and the egress guardrail; the
	// function label under the policy that opens the internet for function pods.
	want := map[string]string{
		"proxcloud.io/project": "p1", "proxcloud.io/service": testSvc, "app.kubernetes.io/managed-by": "proxcloud",
		"proxcloud.io/tenant": "true", "proxcloud.io/function-tenant": "true",
	}
	if !reflect.DeepEqual(f.namespaces["fn-p1"], want) {
		t.Errorf("namespace labels: %v", f.namespaces["fn-p1"])
	}
	if got := dig(f.quotas["fn-p1"], "spec", "hard"); !reflect.DeepEqual(got, map[string]any{"count/deployments.apps": "4", "requests.cpu": "2", "requests.memory": "2048Mi", "pods": "24"}) {
		t.Errorf("quota: %v", got)
	}
	// Nothing else: no registry credential is copied into the namespace, and on a cluster of its own no role is
	// bound (the identity's rights are cluster-wide there).
	if len(f.core) != 0 || len(f.bindings) != 0 {
		t.Errorf("written beside the namespace and its quota: %v %v", f.core, f.bindings)
	}
	// Again, with other limits: the quota follows.
	if err := s.EnsureNamespace(ctx, "p1", sandbox.FunctionQuota{Functions: 1, VCPUs: 1, RAMMb: 512}); err != nil || digStr(f.quotas["fn-p1"], "spec", "hard", "count/deployments.apps") != "1" {
		t.Errorf("a second time: %v, quota %v", err, f.quotas["fn-p1"])
	}

	// Teardown refuses a namespace of another service, removes its own, and tolerates absence.
	f.namespaces["fn-p2"] = map[string]string{"proxcloud.io/project": "p2", "proxcloud.io/service": "other", "app.kubernetes.io/managed-by": "proxcloud"}
	if err := s.DeleteNamespace(ctx, "p2"); err == nil || f.namespaces["fn-p2"] == nil {
		t.Errorf("foreign namespace deleted (err %v)", err)
	}
	if err := s.DeleteNamespace(ctx, "p1"); err != nil || f.namespaces["fn-p1"] != nil {
		t.Errorf("own namespace: err %v, still there %v", err, f.namespaces["fn-p1"] != nil)
	}
	if err := s.DeleteNamespace(ctx, "p1"); err != nil {
		t.Errorf("absent namespace: %v", err)
	}
}

func TestApplyKeepsScale(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	if err := s.Apply(ctx, testFunction(), testSnap, nil, ""); err != nil {
		t.Fatal(err)
	}
	if n := f.replicas(); n != 1 {
		t.Fatalf("new function scale = %d; want 1 (a deploy warms one instance)", n)
	}
	if err := s.SetScale(ctx, "p1", testID, 0); err != nil {
		t.Fatal(err)
	}
	// A second deploy must not wake a function that is at zero.
	next := testSnap
	next.Image = "nginx:1.28"
	if err := s.Apply(ctx, testFunction(), next, nil, ""); err != nil {
		t.Fatal(err)
	}
	if n := f.replicas(); n != 0 {
		t.Fatalf("scale after redeploy = %d, want 0", n)
	}
	if img := digStr(f.deps["fn-p1/"+testID]["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any), "image"); img != "nginx:1.28" {
		t.Errorf("image = %s", img)
	}
}

func TestScaleGuards(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	if err := s.SetScale(ctx, "p1", testID, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetScale absent: %v", err)
	}
	f.calls = nil
	if err := s.SetScale(ctx, "p1", "kube-dns", 1); !errors.Is(err, ErrNotFound) || len(f.calls) != 0 {
		t.Errorf("SetScale on a non-function name: %v, calls %v", err, f.calls)
	}
	_ = s.Apply(ctx, testFunction(), testSnap, nil, "")
	for _, n := range []int{-1, sandbox.MaxFunctionInstances + 1} {
		if err := s.SetScale(ctx, "p1", testID, n); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("SetScale(%d): %v, want a range error", n, err)
		}
	}
	// The wake path is one round trip: the scale subresource, and no read of the Deployment before it.
	f.calls = nil
	if err := s.SetScale(ctx, "p1", testID, 3); err != nil || f.replicas() != 3 || !slices.Equal(f.calls, []string{"patch deployments/scale"}) {
		t.Errorf("SetScale: %v, replicas %d, calls %v", err, f.replicas(), f.calls)
	}
}

func TestSetSuspended(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	if err := s.SetSuspended(ctx, "p1", testID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
	_ = s.Apply(ctx, testFunction(), testSnap, nil, "")
	ann := func() any { return dig(f.deps["fn-p1/"+testID], "metadata", "annotations", AnnotationSuspended) }
	if err := s.SetSuspended(ctx, "p1", testID, true); err != nil || ann() != "true" {
		t.Fatalf("suspend: %v, annotation %v", err, ann())
	}
	if err := s.SetSuspended(ctx, "p1", testID, false); err != nil || ann() != nil {
		t.Fatalf("resume: %v, annotation %v", err, ann())
	}
	if n := f.replicas(); n != 1 {
		t.Errorf("SetSuspended changed the scale to %d", n)
	}
}

func TestStatus(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	if _, err := s.Status(ctx, "p1", testID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
	_ = s.Apply(ctx, testFunction(), testSnap, nil, "")
	dep := f.deps["fn-p1/"+testID]
	dep["metadata"].(map[string]any)["generation"] = float64(2)
	set := func(status map[string]any) { dep["status"] = status }
	check := func(name string, want Status) {
		t.Helper()
		got, err := s.Status(ctx, "p1", testID)
		if err != nil || got.Desired != want.Desired || got.Ready != want.Ready || got.RolloutDone != want.RolloutDone || got.Failed != want.Failed || got.Reason != want.Reason {
			t.Errorf("%s: got %+v (err %v), want %+v", name, got, err, want)
		}
		if got.Reason != "" && !slices.Contains(Reasons, got.Reason) {
			t.Errorf("%s: reason %q is not one of the fixed set", name, got.Reason)
		}
	}
	timedOut := []any{map[string]any{"type": "Progressing", "status": "False", "reason": "ProgressDeadlineExceeded"}}

	// The controller has not seen the new spec: the old status (even a failure) says nothing.
	set(map[string]any{"observedGeneration": float64(1), "replicas": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(1), "readyReplicas": float64(1), "conditions": timedOut})
	check("not observed", Status{Desired: 1, Ready: 1, Reason: ReasonRollout})

	// New pod not ready yet, old one still serving.
	set(map[string]any{"observedGeneration": float64(2), "replicas": float64(2), "updatedReplicas": float64(1), "availableReplicas": float64(1), "readyReplicas": float64(1)})
	f.pods = []map[string]any{pod("old", "n1", "running", true), pod("new", "", "", false)}
	check("unscheduled", Status{Desired: 1, Ready: 1, Reason: ReasonUnplaced})
	f.pods[1] = pod("new", "n1", "", false)
	check("starting", Status{Desired: 1, Ready: 1, Reason: ReasonStarting})
	f.pods[1] = pod("new", "n1", "waiting:ImagePullBackOff", false)
	check("pull", Status{Desired: 1, Ready: 1, Reason: ReasonPull})
	// With registry credentials of its own, a failed pull is about them; the kubelet's words
	// (they can quote the registry) are never passed on.
	tpl := dig(dep, "spec", "template", "metadata", "annotations").(map[string]any)
	tpl[AnnotationRegistryAuth] = "v1"
	f.pods[1]["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["state"].(map[string]any)["waiting"].(map[string]any)["message"] = "unauthorized: hunter2"
	check("pull with credentials", Status{Desired: 1, Ready: 1, Reason: ReasonPullPrivate})
	if st, _ := s.Status(ctx, "p1", testID); strings.Contains(st.Reason, "hunter2") {
		t.Errorf("the reason repeats the kubelet: %q", st.Reason)
	}
	delete(tpl, AnnotationRegistryAuth)
	f.pods[1] = pod("new", "n1", "waiting:CrashLoopBackOff", false)
	check("crash", Status{Desired: 1, Ready: 1, Reason: ReasonCrash})
	f.pods[1] = pod("new", "n1", "terminated", false)
	check("exited", Status{Desired: 1, Ready: 1, Reason: ReasonCrash})
	f.pods[1] = pod("new", "n1", "running", false)
	check("not listening", Status{Desired: 1, Ready: 1, Reason: ReasonNotListening})
	if got := f.selectors[len(f.selectors)-1]; got != "proxcloud.io/function-id="+testID {
		t.Errorf("pod selector %q", got)
	}

	// Deadline passed: failed, and the pod still says why.
	set(map[string]any{"observedGeneration": float64(2), "replicas": float64(2), "updatedReplicas": float64(1), "availableReplicas": float64(1), "readyReplicas": float64(1), "conditions": timedOut})
	check("failed", Status{Desired: 1, Ready: 1, Failed: true, Reason: ReasonNotListening})
	f.pods = f.pods[:1]
	check("failed, no pod to explain", Status{Desired: 1, Ready: 1, Failed: true, Reason: ReasonDeadline})

	// Quota (or admission) stopped the pod from being created.
	set(map[string]any{"observedGeneration": float64(2), "replicas": float64(0), "conditions": []any{map[string]any{"type": "ReplicaFailure", "status": "True", "reason": "FailedCreate"}}})
	f.pods = nil
	check("replica failure", Status{Desired: 1, Reason: ReasonNotCreated})

	// Rolled out.
	set(map[string]any{"observedGeneration": float64(2), "replicas": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(1), "readyReplicas": float64(1)})
	check("done", Status{Desired: 1, Ready: 1, RolloutDone: true})

	// At zero there is nothing to roll: done at once.
	_ = s.SetScale(ctx, "p1", testID, 0)
	set(map[string]any{"observedGeneration": float64(2)})
	check("zero", Status{RolloutDone: true})
}

// What a customer is shown of a start that does not succeed is one of a fixed set of sentences. What the cluster
// said (the kubelet about a pull, the scheduler about a machine, the controller about a refused pod) is kept beside
// it for the operator's log and is in no answer: it names registries' replies, machines, addresses and other
// tenants' numbers.
func TestStatusReasonIsFixedAndTheClustersWordsStayWithTheOperator(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	_ = s.Apply(ctx, testFunction(), testSnap, nil, "")
	dep := f.deps["fn-p1/"+testID]
	rolling := map[string]any{"replicas": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(0)}
	withState := func(kind, reason, message string) map[string]any {
		p := pod("new", "worker-7", kind+":"+reason, false)
		p["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["state"].(map[string]any)[kind].(map[string]any)["message"] = message
		return p
	}
	unplaced := pod("new", "", "", false)
	unplaced["status"].(map[string]any)["conditions"] = []any{
		map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/4 nodes are available: 1 Insufficient cpu on worker-7, 3 node(s) had untolerated taint"},
	}
	for name, c := range map[string]struct {
		status map[string]any
		pods   []map[string]any
		reason string
		detail string
	}{
		"a pull the registry refused": {rolling, []map[string]any{withState("waiting", "ErrImagePull", `failed to pull and unpack image "10.20.0.5:5000/x": failed to resolve reference: dial tcp 10.20.0.5:5000: connect: connection refused`)},
			ReasonPull, "10.20.0.5:5000"},
		"a container that keeps exiting": {rolling, []map[string]any{withState("waiting", "CrashLoopBackOff", "back-off 40s restarting failed container=main pod=new_fn-p1(1f0c)")},
			ReasonCrash, "back-off 40s"},
		"a container that could not start": {rolling, []map[string]any{withState("terminated", "StartError", `exec: "/bin/serve --token=hunter2": no such file`)},
			ReasonCrash, "hunter2"},
		"no machine with room": {rolling, []map[string]any{unplaced}, ReasonUnplaced, "Insufficient cpu on worker-7"},
		"a pod the quota refused": {map[string]any{"replicas": float64(0), "conditions": []any{map[string]any{"type": "ReplicaFailure", "status": "True", "reason": "FailedCreate",
			"message": `pods "fn-0123456789abcdef-7d9" is forbidden: exceeded quota: proxcloud-function, requested: requests.cpu=2, used: requests.cpu=7, limited: requests.cpu=8`}}},
			nil, ReasonNotCreated, "exceeded quota"},
	} {
		dep["status"], f.pods = c.status, c.pods
		st, err := s.Status(ctx, "p1", testID)
		if err != nil || st.Reason != c.reason || !slices.Contains(Reasons, st.Reason) || !strings.Contains(st.Detail, c.detail) {
			t.Errorf("%s: %+v (err %v), want reason %q and the cluster's words in Detail", name, st, err, c.reason)
		}
		// Whatever is made of a Status for an answer, the cluster's words are not in it.
		if raw, _ := json.Marshal(st); strings.Contains(string(raw), c.detail) || strings.Contains(st.Reason, c.detail) {
			t.Errorf("%s: the cluster's words are in what is answered: %s", name, raw)
		}
	}
	// A rollout that is done says nothing, and keeps nothing of an earlier failure.
	dep["status"], f.pods = map[string]any{"replicas": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(1), "readyReplicas": float64(1),
		"conditions": []any{map[string]any{"type": "ReplicaFailure", "status": "True", "message": "exceeded quota"}}}, nil
	if st, _ := s.Status(ctx, "p1", testID); !st.RolloutDone || st.Reason != "" || st.Detail != "" {
		t.Errorf("done: %+v", st)
	}
}

func TestList(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	_ = s.Apply(ctx, testFunction(), testSnap, nil, "")
	f.deps["fn-p1/"+testID]["status"] = map[string]any{"replicas": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(1), "readyReplicas": float64(1)}
	gone := testFunction()
	gone.ID = "fn-00000000000000aa"
	_ = s.Apply(ctx, gone, testSnap, nil, "")
	f.deps["fn-p1/"+gone.ID]["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-16T00:00:00Z"

	got, err := s.List(ctx, "p1")
	if err != nil || len(got) != 1 || !got[testID].RolloutDone || got[testID].Ready != 1 {
		t.Fatalf("List = %+v, %v", got, err)
	}
	if sel := f.selectors[len(f.selectors)-1]; sel != "proxcloud.io/project=p1,app.kubernetes.io/managed-by=proxcloud,proxcloud.io/service="+testSvc {
		t.Errorf("selector %q", sel)
	}

	// ListAll: every function Deployment of the service on the cluster, with its project. Not one
	// of another service, nor one outside its project's function namespace.
	other := testFunction()
	other.ID, other.ProjectID = "fn-00000000000000bb", "p2"
	_ = s.Apply(ctx, other, testSnap, nil, "")
	foreign := BuildDeployment(testConfig(), "svc-other", Function{ID: "fn-00000000000000cc", ProjectID: "p3"}, testSnap)
	f.deps["fn-p3/fn-00000000000000cc"] = foreign
	moved := BuildDeployment(testConfig(), testSvc, Function{ID: "fn-00000000000000dd", ProjectID: "p1"}, testSnap)
	moved["metadata"].(map[string]any)["namespace"] = "default"
	f.deps["default/fn-00000000000000dd"] = moved
	// A sandbox namespace holds no function: a Deployment there with a function's name and labels is not one.
	inSandboxNS := BuildDeployment(testConfig(), testSvc, Function{ID: "fn-00000000000000ee", ProjectID: "p1"}, testSnap)
	inSandboxNS["metadata"].(map[string]any)["namespace"] = "sb-p1"
	f.deps["sb-p1/fn-00000000000000ee"] = inSandboxNS
	f.calls = nil
	all, err := s.ListAll(ctx)
	if err != nil || len(all) != 2 || all[testID].ProjectID != "p1" || all[testID].Ready != 1 || all[other.ID].ProjectID != "p2" || all[other.ID].Desired != 1 {
		t.Fatalf("ListAll = %+v, %v", all, err)
	}
	if sel := f.selectors[len(f.selectors)-1]; sel != "app.kubernetes.io/managed-by=proxcloud,proxcloud.io/service="+testSvc {
		t.Errorf("ListAll selector %q", sel)
	}
	// On a cluster of its own that is one call at the cluster scope.
	if !slices.Equal(f.calls, []string{"list deployments (cluster)"}) {
		t.Errorf("ListAll calls %v", f.calls)
	}
}

func TestLogs(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	if _, err := s.Logs(ctx, "p1", testID, 100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
	_ = s.Apply(ctx, testFunction(), testSnap, nil, "")
	if out, err := s.Logs(ctx, "p1", testID, 100); err != nil || out != "" {
		t.Fatalf("at zero: %q, %v", out, err)
	}
	f.pods = []map[string]any{pod("a", "n1", "running", true)}
	if out, err := s.Logs(ctx, "p1", testID, 100); err != nil || out != "log of a/main\n" {
		t.Fatalf("one pod: %q, %v", out, err)
	}
	// During a rollout: both pods, labelled; one that has not started yet is skipped.
	f.pods = append(f.pods, pod("b", "n1", "running", true), pod("c", "n1", "waiting:ContainerCreating", false))
	f.logErr["c"] = &kamajik8s.APIError{Status: http.StatusBadRequest, Message: "container is waiting to start"}
	if out, err := s.Logs(ctx, "p1", testID, 100); err != nil || out != "==> a <==\nlog of a/main\n==> b <==\nlog of b/main\n" {
		t.Fatalf("three pods: %q, %v", out, err)
	}
	f.logErr["b"] = &kamajik8s.APIError{Status: http.StatusForbidden}
	if _, err := s.Logs(ctx, "p1", testID, 100); err == nil {
		t.Fatal("a real log error was swallowed")
	}
}

func TestDelete(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	if err := s.Delete(ctx, "p1", testID); err != nil {
		t.Fatalf("absent: %v", err)
	}
	_ = s.Apply(ctx, testFunction(), testSnap, nil, "")
	// Another service's Deployment with a function-shaped name is left alone.
	foreign := "fn-00000000000000ff"
	f.deps["fn-p1/"+foreign] = map[string]any{"metadata": map[string]any{"name": foreign, "labels": map[string]any{"proxcloud.io/service": "other"}}}
	if err := s.Delete(ctx, "p1", foreign); err != nil || f.deps["fn-p1/"+foreign] == nil {
		t.Errorf("foreign deployment: err %v, deleted %v", err, f.deps["fn-p1/"+foreign] == nil)
	}
	if err := s.Delete(ctx, "p1", testID); err != nil || f.deps["fn-p1/"+testID] != nil {
		t.Errorf("own deployment: err %v", err)
	}
	// A function of project p1 is not reachable through another project.
	_ = s.Apply(ctx, testFunction(), testSnap, nil, "")
	if _, err := s.Status(ctx, "p2", testID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-project status: %v", err)
	}
}

// A function's secrets live in one Secret, applied before the Deployment that reads it and owned
// by that Deployment; without secrets the Secret is emptied, and a delete empties it last.
func TestApplySecrets(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	key := "secrets/fn-p1/" + testID
	owner := []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": testID, "uid": "uid-" + testID}}

	// No secrets, no Secret: nothing is created for a function that has none.
	if err := s.Apply(ctx, testFunction(), testSnap, nil, ""); err != nil || f.core[key] != nil || len(f.ops) != 1 {
		t.Fatalf("apply without secrets: err %v, secret %v, ops %v", err, f.core[key], f.ops)
	}
	_ = s.Delete(ctx, "p1", testID)

	// A new function: the Secret first (no instance may start without it), then the Deployment,
	// then the Secret again with its owner.
	snap := testSnap
	snap.SecretNames, snap.SecretsVersion = []string{"OTHER", "TOKEN"}, "v1"
	f.ops = nil
	if err := s.Apply(ctx, testFunction(), snap, map[string]string{"TOKEN": "hunter2", "OTHER": "x"}, ""); err != nil {
		t.Fatal(err)
	}
	if want := []string{"apply secrets", "apply deployment", "apply secrets"}; !reflect.DeepEqual(f.ops, want) {
		t.Fatalf("ops = %v, want %v", f.ops, want)
	}
	sec := f.core[key]
	if !reflect.DeepEqual(sec["data"], map[string]any{"TOKEN": "aHVudGVyMg==", "OTHER": "eA=="}) || sec["type"] != "Opaque" || !reflect.DeepEqual(dig(sec, "metadata", "ownerReferences"), owner) ||
		digStr(sec, "metadata", "labels", LabelID) != testID || digStr(sec, "metadata", "labels", sandbox.LabelManagedBy) != sandbox.ManagedByValue {
		t.Fatalf("secret: %v", sec)
	}

	// A secret is removed, the other stays: what is applied names the one that is left and nothing of the one
	// that went, in data, so the apply takes the key away. Sent as stringData it would stay in the Secret, and in
	// the environment of every instance started afterwards.
	snap.SecretNames, snap.SecretsVersion = []string{"OTHER"}, "v2"
	f.ops = nil
	if err := s.Apply(ctx, testFunction(), snap, map[string]string{"OTHER": "x"}, ""); err != nil {
		t.Fatal(err)
	}
	sec = f.core[key]
	if want := []string{"apply secrets", "apply deployment"}; !reflect.DeepEqual(f.ops, want) || !reflect.DeepEqual(sec["data"], map[string]any{"OTHER": "eA=="}) ||
		!reflect.DeepEqual(dig(sec, "metadata", "ownerReferences"), owner) || sec["stringData"] != nil || sec["type"] != "Opaque" {
		t.Fatalf("ops = %v, secret %v", f.ops, sec)
	}
	if raw, _ := json.Marshal(sec); strings.Contains(string(raw), "TOKEN") || strings.Contains(string(raw), "aHVudGVyMg") {
		t.Fatalf("the removed secret is still in what was applied: %s", raw)
	}

	// All secrets removed: the Deployment stops reading the Secret, then it is emptied.
	f.ops = nil
	if err := s.Apply(ctx, testFunction(), testSnap, nil, ""); err != nil {
		t.Fatal(err)
	}
	if want := []string{"apply deployment", "patch secrets"}; !reflect.DeepEqual(f.ops, want) || f.core[key]["data"] != nil {
		t.Fatalf("ops = %v, secret %v", f.ops, f.core[key])
	}

	// A delete that fails leaves the function as it was: its Secret still has the values the
	// next instance starts with.
	_ = s.Apply(ctx, testFunction(), snap, map[string]string{"OTHER": "x"}, "")
	f.delErr = &kamajik8s.APIError{Status: http.StatusServiceUnavailable}
	if err := s.Delete(ctx, "p1", testID); err == nil || f.core[key]["data"] == nil || f.deps["fn-p1/"+testID] == nil {
		t.Fatalf("failed delete: err %v, secret %v", err, f.core[key])
	}
	// Delete removes the Deployment (the Secret's owner), then empties the Secret; the Secret is
	// emptied also for a function id whose Deployment is already gone.
	f.delErr, f.ops = nil, nil
	if err := s.Delete(ctx, "p1", testID); err != nil || f.core[key]["data"] != nil || !reflect.DeepEqual(f.ops, []string{"delete deployment", "patch secrets"}) {
		t.Fatalf("delete: err %v, secret %v, ops %v", err, f.core[key], f.ops)
	}
	f.core[key]["data"] = map[string]any{"LEFT": "eA=="}
	if err := s.Delete(ctx, "p1", testID); err != nil || f.core[key]["data"] != nil {
		t.Fatalf("delete without a deployment: err %v, secret %v", err, f.core[key])
	}
	if err := s.Delete(ctx, "p1", "../secrets"); err != nil || len(f.core) != 1 {
		t.Fatalf("an id that is not one must reach nothing: %v", err)
	}
}

// A function's registry credentials live in a pull secret of their own, applied and owned like its
// Secret and next to it; without credentials it is emptied to a docker config that has none (a
// pull secret cannot be without one).
func TestApplyRegistryAuth(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	key, pullKey := "secrets/fn-p1/"+testID, "secrets/fn-p1/"+testID+"-registry"
	config := func() string {
		b, _ := base64.StdEncoding.DecodeString(digStr(f.core[pullKey], "data", ".dockerconfigjson"))
		return string(b)
	}
	owner := []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": testID, "uid": "uid-" + testID}}
	snap := testSnap
	snap.Image, snap.RegistryAuth = "ghcr.io/acme/api:1", &RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v1"}
	snap.SecretNames, snap.SecretsVersion = []string{"TOKEN"}, "v1"

	// A new function: both Secrets, the Deployment, both again with their owner.
	if err := s.Apply(ctx, testFunction(), snap, map[string]string{"TOKEN": "x"}, "hunter2"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"apply secrets", "apply secrets", "apply deployment", "apply secrets", "apply secrets"}; !reflect.DeepEqual(f.ops, want) {
		t.Fatalf("ops = %v, want %v", f.ops, want)
	}
	if !strings.Contains(config(), `"password":"hunter2"`) || !reflect.DeepEqual(dig(f.core[pullKey], "metadata", "ownerReferences"), owner) || !reflect.DeepEqual(dig(f.core[key], "metadata", "ownerReferences"), owner) ||
		!reflect.DeepEqual(f.core[key]["data"], map[string]any{"TOKEN": "eA=="}) || f.core[pullKey]["type"] != "kubernetes.io/dockerconfigjson" || f.core[key]["type"] != "Opaque" {
		t.Fatalf("pull secret %v (config %s), secret %v", f.core[pullKey], config(), f.core[key])
	}

	// New credentials on an existing function: one apply of each, then the Deployment.
	snap.RegistryAuth = &RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v2"}
	f.ops = nil
	if err := s.Apply(ctx, testFunction(), snap, map[string]string{"TOKEN": "x"}, "other"); err != nil || !strings.Contains(config(), `"password":"other"`) ||
		!reflect.DeepEqual(f.ops, []string{"apply secrets", "apply secrets", "apply deployment"}) {
		t.Fatalf("replace: err %v, ops %v, config %s", err, f.ops, config())
	}

	// Credentials removed, secrets kept: the Deployment stops naming the pull secret, then it is
	// emptied. The function's Secret is untouched.
	snap.RegistryAuth, f.ops = nil, nil
	if err := s.Apply(ctx, testFunction(), snap, map[string]string{"TOKEN": "x"}, ""); err != nil || config() != `{"auths":{}}` ||
		!reflect.DeepEqual(f.ops, []string{"apply secrets", "apply deployment", "patch secrets"}) || !reflect.DeepEqual(f.core[key]["data"], map[string]any{"TOKEN": "eA=="}) {
		t.Fatalf("remove: err %v, ops %v, config %s", err, f.ops, config())
	}

	// Delete empties both, after the Deployment is gone.
	snap.RegistryAuth = &RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v3"}
	_ = s.Apply(ctx, testFunction(), snap, map[string]string{"TOKEN": "x"}, "hunter2")
	f.ops = nil
	if err := s.Delete(ctx, "p1", testID); err != nil || config() != `{"auths":{}}` || f.core[key]["data"] != nil ||
		!reflect.DeepEqual(f.ops, []string{"delete deployment", "patch secrets", "patch secrets"}) {
		t.Fatalf("delete: err %v, ops %v, config %s", err, f.ops, config())
	}
}

// The digest a rollback deploys is the one the container runtime resolved for a ready instance: the manifest
// digest of its repository, never the id of the image's config, and never a pod that is not serving.
func TestImageDigest(t *testing.T) {
	s, f := testService()
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("ab", 32)
	with := func(name string, ready bool, imageID string) map[string]any {
		p := pod(name, "n1", "running", ready)
		p["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["imageID"] = imageID
		return p
	}
	for name, c := range map[string]struct {
		pods []map[string]any
		want string
	}{
		"no instance":               {nil, ""},
		"a ready instance":          {[]map[string]any{with("a", true, "ghcr.io/acme/api@"+digest)}, digest},
		"only one that is starting": {[]map[string]any{with("a", false, "ghcr.io/acme/api@"+digest)}, ""},
		"the id of the config":      {[]map[string]any{with("a", true, digest)}, ""},
		"no digest":                 {[]map[string]any{with("a", true, "ghcr.io/acme/api:1")}, ""},
		"something after it":        {[]map[string]any{with("a", true, "ghcr.io/acme/api@"+digest+"0")}, ""},
		"the ready one of two":      {[]map[string]any{with("a", false, "ghcr.io/acme/api@sha256:"+strings.Repeat("00", 32)), with("b", true, "ghcr.io/acme/api@"+digest)}, digest},
	} {
		f.pods = c.pods
		if got, err := s.ImageDigest(ctx, "p1", testID); err != nil || got != c.want {
			t.Errorf("%s: %q (err %v), want %q", name, got, err, c.want)
		}
	}
	if sel := f.selectors[len(f.selectors)-1]; sel != "proxcloud.io/function-id="+testID {
		t.Errorf("pod selector %q", sel)
	}
	f.calls = nil
	if _, err := s.ImageDigest(ctx, "p1", "x,"+sandbox.LabelSandboxID); !errors.Is(err, ErrNotFound) || len(f.calls) != 0 {
		t.Errorf("an id that is not one reached a selector: %v, calls %v", err, f.calls)
	}
}

// What the portal's identity may do in a function namespace is the chart's ClusterRole proxcloud-function-namespace,
// held to these verbs (functionRoleRules in internal/cloud/sandbox/chart_functions_test.go is the same list, read
// from the chart's render). Everything the service does on the cluster, in the whole life of a function with secrets
// and registry credentials, is among them, and each of them is something it does: in particular it never reads a
// Secret, never deletes one, never watches, never reads a scale, and reads neither a single pod nor a quota.
var roleVerbs = map[string][]string{
	"deployments":       {"get", "list", "create", "patch", "delete"},
	"deployments/scale": {"patch"},
	"secrets":           {"create", "patch"},
	"pods":              {"list"},
	"pods/log":          {"get"},
	"resourcequotas":    {"create", "patch"},
}

func lifecycle(t *testing.T, s *Service, f *fakeAPI) {
	t.Helper()
	ctx := context.Background()
	snap := testSnap
	snap.Image, snap.RegistryAuth = "ghcr.io/acme/api:1", &RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v1"}
	snap.SecretNames, snap.SecretsVersion = []string{"TOKEN"}, "v1"
	steps := []func() error{
		func() error { return s.EnsureNamespace(ctx, "p1", testQuota) },
		func() error { return s.Apply(ctx, testFunction(), snap, map[string]string{"TOKEN": "x"}, "hunter2") },
		func() error { return s.Apply(ctx, testFunction(), testSnap, nil, "") }, // secrets and credentials removed
		func() error { return s.Apply(ctx, testFunction(), snap, map[string]string{"TOKEN": "y"}, "hunter3") },
		func() error { return s.SetScale(ctx, "p1", testID, 0) },
		func() error { return s.SetScale(ctx, "p1", testID, 2) },
		func() error { return s.SetSuspended(ctx, "p1", testID, true) },
		func() error { return s.SetSuspended(ctx, "p1", testID, false) },
		func() error { _, err := s.Status(ctx, "p1", testID); return err },
		func() error { _, err := s.ImageDigest(ctx, "p1", testID); return err },
		func() error { _, err := s.List(ctx, "p1"); return err },
		func() error { _, err := s.ListAll(ctx); return err },
		func() error { _, err := s.Logs(ctx, "p1", testID, 10); return err },
		func() error { return s.Delete(ctx, "p1", testID) },
		func() error { return s.Delete(ctx, "p1", testID) }, // again: already gone
		func() error { return s.DeleteNamespace(ctx, "p1") },
	}
	f.pods = []map[string]any{pod("a", "n1", "running", false)}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
}

func TestClusterCallsAreTheRolesVerbs(t *testing.T) {
	for _, shared := range []bool{false, true} {
		cfg := testConfig()
		cfg.Shared = shared
		f := newFake()
		lifecycle(t, NewWithAPI(f, cfg, testSvc), f)
		used := map[string]bool{}
		for _, call := range f.calls {
			used[call] = true
			verb, resource, _ := strings.Cut(call, " ")
			switch {
			case resource == "namespaces (cluster)":
				// The identity's own cluster-wide rights, as for sandboxes.
			case resource == "rolebindings (cluster)" && shared:
				// Shared: it may write RoleBindings, and the admission policy holds which.
			case resource == "deployments (cluster)" && !shared && verb == "list":
				// A cluster of its own: its rules hold cluster-wide there.
			case !slices.Contains(roleVerbs[resource], verb):
				t.Errorf("shared=%v: the service did %q, which the function role does not allow", shared, call)
			}
		}
		// And it needs what it is given, as far as this package goes: the list a reviewer of the role reads.
		want := []string{
			"create deployments", "delete deployments", "get deployments", "list deployments", "patch deployments",
			"patch deployments/scale", "create secrets", "patch secrets", "list pods", "get pods/log", "create resourcequotas", "patch resourcequotas",
		}
		for _, w := range want {
			if !used[w] && !(w == "list deployments" && !shared && used["list deployments (cluster)"]) {
				t.Errorf("shared=%v: the lifecycle never did %q", shared, w)
			}
		}
		granted := 0
		for _, verbs := range roleVerbs {
			granted += len(verbs)
		}
		if granted != len(want) {
			t.Errorf("the role grants %d verbs and the lifecycle needs %d: every right of the role is one the service uses", granted, len(want))
		}
		if shared && used["list deployments (cluster)"] {
			t.Error("a shared cluster was asked for every Deployment on it")
		}
	}
	// The surface itself: a method that reads or deletes a Secret, watches, or reads a scale has no way in.
	var names []string
	typ := reflect.TypeOf((*K8sAPI)(nil)).Elem()
	for i := range typ.NumMethod() {
		names = append(names, typ.Method(i).Name)
	}
	want := []string{"ApplyCR", "ApplyCore", "ApplyResourceQuota", "DeleteCR", "DeleteNamespace", "EnsureNamespace", "GetCR", "GetNamespace", "ListCRs",
		"ListCRsAllNamespaces", "ListNamespaces", "ListPods", "MergePatchCR", "MergePatchCore", "PodLogs", "SetScale"}
	if !slices.Equal(names, want) {
		t.Errorf("the cluster surface is %v, want %v: a new call is a new right of the portal's identity in every function namespace", names, want)
	}
}

// sharedRBAC is the API server as the portal's identity meets it on a shared cluster once the chart carries the
// function objects: cluster-wide it may read and write namespaces and write RoleBindings, and everything else it may
// do only where a RoleBinding gives it a role. The admission policy lets it bind, to itself alone, the role that fits
// the namespace: the function role in a tenant's fn-*, the sandbox role in a tenant's sb-*, nothing anywhere else.
type sharedRBAC struct {
	*fakeAPI
	refused []string
}

func (a *sharedRBAC) deny(what string) error {
	a.refused = append(a.refused, what)
	return &kamajik8s.APIError{Status: http.StatusForbidden, Message: what + " is forbidden"}
}

// role is the role the identity is bound to in ns ("" = none).
func (a *sharedRBAC) role(ns string) string {
	return digStr(a.bindings[ns+"/"+sandbox.SharedAccount], "roleRef", "name")
}

func (a *sharedRBAC) ApplyCR(ctx context.Context, g, v, plural string, obj map[string]any) error {
	ns := digStr(obj, "metadata", "namespace")
	if plural != "rolebindings" {
		if a.role(ns) != sandbox.FunctionRole {
			return a.deny("patch " + plural + " in " + ns)
		}
		return a.fakeAPI.ApplyCR(ctx, g, v, plural, obj)
	}
	subjects, _ := obj["subjects"].([]any)
	self := len(subjects) == 1 && reflect.DeepEqual(subjects[0], map[string]any{"kind": "ServiceAccount", "name": sandbox.SharedAccount, "namespace": sandbox.SharedAccountNS})
	fits := strings.HasPrefix(ns, "fn-") && digStr(obj, "roleRef", "name") == sandbox.FunctionRole || strings.HasPrefix(ns, "sb-") && digStr(obj, "roleRef", "name") == sandbox.SharedRole
	if !fits || a.namespaces[ns][sandbox.LabelTenant] != "true" || digStr(obj, "roleRef", "kind") != "ClusterRole" || digStr(obj, "roleRef", "apiGroup") != "rbac.authorization.k8s.io" || !self {
		return a.deny("the admission policy: rolebinding of " + digStr(obj, "roleRef", "name") + " in " + ns)
	}
	return a.fakeAPI.ApplyCR(ctx, g, v, plural, obj)
}

func (a *sharedRBAC) ApplyResourceQuota(ctx context.Context, rq map[string]any) error {
	if ns := digStr(rq, "metadata", "namespace"); a.role(ns) != sandbox.FunctionRole {
		return a.deny("patch resourcequotas in " + ns)
	}
	return a.fakeAPI.ApplyResourceQuota(ctx, rq)
}

func (a *sharedRBAC) ListCRs(ctx context.Context, g, v, plural, ns, sel string) ([]map[string]any, error) {
	if a.role(ns) != sandbox.FunctionRole {
		return nil, a.deny("list " + plural + " in " + ns)
	}
	return a.fakeAPI.ListCRs(ctx, g, v, plural, ns, sel)
}

func (a *sharedRBAC) ListCRsAllNamespaces(context.Context, string, string, string, string) ([]map[string]any, error) {
	return nil, a.deny("list deployments at the cluster scope")
}

// On a shared cluster the identity has no right in a namespace until it is bound there. A function namespace gets
// the function role, never the sandbox role (which holds exec, and no Deployment), and the lists that span projects
// go namespace by namespace over this service's fn-* namespaces: not into a sandbox namespace, not into another
// service's, and never as one read of every Deployment of the platform.
func TestSharedClusterBindsTheFunctionRoleAndListsPerNamespace(t *testing.T) {
	ctx := context.Background()
	api := &sharedRBAC{fakeAPI: newFake()}
	cfg := testConfig()
	cfg.Shared = true
	s := NewWithAPI(api, cfg, testSvc)
	for i, p := range []string{"p1", "p2"} {
		if err := s.EnsureNamespace(ctx, p, testQuota); err != nil {
			t.Fatalf("project %s: %v", p, err)
		}
		rb := api.bindings["fn-"+p+"/proxcloud-sandbox"]
		if !reflect.DeepEqual(rb["roleRef"], map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "proxcloud-function-namespace"}) ||
			!reflect.DeepEqual(rb["subjects"], []any{map[string]any{"kind": "ServiceAccount", "name": "proxcloud-sandbox", "namespace": "sandbox-system"}}) {
			t.Fatalf("fn-%s: the RoleBinding is %v", p, rb)
		}
		if api.quotas["fn-"+p] == nil || api.namespaces["fn-"+p][sandbox.LabelTenant] != "true" || api.namespaces["fn-"+p][LabelTenant] != "true" {
			t.Fatalf("fn-%s: no quota, or not labelled as a tenant's: %v", p, api.namespaces["fn-"+p])
		}
		fn := Function{ID: "fn-000000000000000" + string(rune('1'+i)), ProjectID: p, Host: "api-a1b2c3.sbx.example.com"}
		if err := s.Apply(ctx, fn, testSnap, nil, ""); err != nil {
			t.Fatalf("project %s: %v", p, err)
		}
	}
	// Beside them: the same project's sandbox namespace (bound to the sandbox role), another provider's function
	// namespace, one that is being deleted, and one that only looks like a function namespace.
	api.namespaces["sb-p1"] = map[string]string{sandbox.LabelTenant: "true", sandbox.LabelService: testSvc}
	api.bindings["sb-p1/proxcloud-sandbox"] = map[string]any{"roleRef": map[string]any{"name": sandbox.SharedRole}}
	api.namespaces["fn-other"] = map[string]string{sandbox.LabelTenant: "true", sandbox.LabelService: "svc-2"}
	api.namespaces["fn-unlabelled"] = map[string]string{}
	api.namespaces["kube-system"] = map[string]string{}
	api.calls = nil
	all, err := s.ListAll(ctx)
	if err != nil || len(all) != 2 || all["fn-0000000000000001"].ProjectID != "p1" || all["fn-0000000000000002"].ProjectID != "p2" {
		t.Fatalf("ListAll = %+v, %v", all, err)
	}
	if len(api.refused) != 0 {
		t.Fatalf("the identity was refused: %v", api.refused)
	}
	if want := []string{"list namespaces (cluster)", "list deployments", "list deployments"}; !slices.Equal(api.calls, want) {
		t.Fatalf("ListAll made %v, want %v: one list per function namespace of this service", api.calls, want)
	}
	if got, err := s.List(ctx, "p1"); err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v", got, err)
	}

	// The same calls as on a cluster of the provider's own are refused here: the mode is what holds.
	plain := NewWithAPI(api, testConfig(), testSvc)
	if _, err := plain.ListAll(ctx); err == nil {
		t.Fatal("a cluster-wide list of Deployments passed on a shared cluster")
	}
	if err := plain.EnsureNamespace(ctx, "p3", testQuota); err == nil || api.quotas["fn-p3"] != nil {
		t.Fatalf("a quota was written in a namespace without the RoleBinding: %v", err)
	}
	// And the policy is why the role is the function role: the sandbox role in a function namespace is refused,
	// so is the function role in a sandbox namespace, so is either in a namespace that is no tenant's.
	for _, c := range []struct{ ns, role string }{{"fn-p1", sandbox.SharedRole}, {"sb-p1", sandbox.FunctionRole}, {"fn-unlabelled", sandbox.FunctionRole}, {"kube-system", sandbox.FunctionRole}} {
		err := sandbox.EnsureTenantNamespace(ctx, onlyBind{api}, true, testSvc, "p1", c.ns, c.role, nil)
		var ae *kamajik8s.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
			t.Errorf("binding %s in %s: %v, want the policy's refusal", c.role, c.ns, err)
		}
	}
}

// onlyBind applies the RoleBinding and leaves the namespace as the test made it.
type onlyBind struct{ *sharedRBAC }

func (onlyBind) EnsureNamespace(context.Context, string, map[string]string, map[string]string) error {
	return nil
}
