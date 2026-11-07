package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
)

// fakeAPI is an in-memory sandbox cluster: namespaces, quotas and Sandbox CRs by ns/name.
type fakeAPI struct {
	namespaces map[string]map[string]string
	quotas     map[string]map[string]any
	crs        map[string]map[string]any // "ns/name"
	deletedNS  []string
	execs      []string
	core       map[string]map[string]any // "plural/ns/name"
	secrets    map[string]map[string][]byte
	nodes      []map[string]any // nil = no GPU check data (ListNodes returns none)
	extraPods  []map[string]any
}

func newFake() *fakeAPI {
	return &fakeAPI{namespaces: map[string]map[string]string{}, quotas: map[string]map[string]any{}, crs: map[string]map[string]any{}, core: map[string]map[string]any{}, secrets: map[string]map[string][]byte{}}
}

func (f *fakeAPI) ApplyCore(_ context.Context, plural string, obj map[string]any) error {
	if m, ok := obj["metadata"].(map[string]any); ok && m["uid"] == nil { // the API server assigns one
		m["uid"] = "uid-" + digStr(obj, "metadata", "name")
	}
	f.core[plural+"/"+digStr(obj, "metadata", "namespace")+"/"+digStr(obj, "metadata", "name")] = obj
	return nil
}
func (f *fakeAPI) GetCore(_ context.Context, plural, ns, name string) (map[string]any, error) {
	return f.core[plural+"/"+ns+"/"+name], nil
}
func (f *fakeAPI) DeleteCore(_ context.Context, plural, ns, name string) error {
	delete(f.core, plural+"/"+ns+"/"+name)
	return nil
}
func (f *fakeAPI) ListPods(_ context.Context, ns, _ string) ([]map[string]any, error) {
	var out []map[string]any
	for k, v := range f.core {
		if strings.HasPrefix(k, "pods/"+ns+"/") {
			out = append(out, v)
		}
	}
	return out, nil
}
func (f *fakeAPI) ExecStream(_ context.Context, ns, pod, container string, cmd []string, w io.Writer, _ int) (int, string, error) {
	f.execs = append(f.execs, ns+"/"+pod+"/"+container+":"+strings.Join(cmd, " "))
	_, _ = w.Write([]byte("layer"))
	return 0, "", nil
}
func (f *fakeAPI) ListCRsAllNamespaces(_ context.Context, _, _, _, _ string) ([]map[string]any, error) {
	var out []map[string]any
	for _, v := range f.crs {
		out = append(out, v)
	}
	return out, nil
}
func (f *fakeAPI) ListNodes(_ context.Context) ([]map[string]any, error) { return f.nodes, nil }
func (f *fakeAPI) ListPodsAllNamespaces(_ context.Context, _ string) ([]map[string]any, error) {
	var out []map[string]any
	for k, v := range f.core {
		if strings.HasPrefix(k, "pods/") {
			out = append(out, v)
		}
	}
	return append(out, f.extraPods...), nil
}
func (f *fakeAPI) GetSecretData(_ context.Context, ns, name string) (map[string][]byte, error) {
	return f.secrets[ns+"/"+name], nil
}

func (f *fakeAPI) EnsureNamespace(_ context.Context, name string, labels map[string]string) error {
	f.namespaces[name] = labels
	return nil
}
func (f *fakeAPI) GetNamespace(_ context.Context, name string) (map[string]any, error) {
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
	delete(f.namespaces, name)
	f.deletedNS = append(f.deletedNS, name)
	return nil
}
func (f *fakeAPI) ApplyResourceQuota(_ context.Context, rq map[string]any) error {
	f.quotas[digStr(rq, "metadata", "namespace")] = rq
	return nil
}
func (f *fakeAPI) ApplyCR(_ context.Context, _, _, _ string, obj map[string]any) error {
	f.crs[digStr(obj, "metadata", "namespace")+"/"+digStr(obj, "metadata", "name")] = obj
	return nil
}
func (f *fakeAPI) GetCR(_ context.Context, _, _, _, ns, name string) (map[string]any, error) {
	return f.crs[ns+"/"+name], nil
}
func (f *fakeAPI) DeleteCR(_ context.Context, _, _, _, ns, name string) error {
	delete(f.crs, ns+"/"+name)
	return nil
}
func (f *fakeAPI) MergePatchCR(_ context.Context, _, _, _, ns, name string, patch map[string]any) (map[string]any, error) {
	obj := f.crs[ns+"/"+name]
	spec, _ := obj["spec"].(map[string]any)
	for k, v := range patch["spec"].(map[string]any) {
		spec[k] = v
	}
	return obj, nil
}
func (f *fakeAPI) PodLogs(_ context.Context, ns, pod, container string, tail int) (string, error) {
	return "2026-11-05T10:00:00Z hello\n", nil
}

func (f *fakeAPI) Exec(_ context.Context, ns, pod, container string, cmd []string, stdin []byte, _ int) (*kamajik8s.ExecResult, error) {
	f.execs = append(f.execs, ns+"/"+pod+"/"+container+":"+strings.Join(cmd, " ")+"<"+string(stdin))
	return &kamajik8s.ExecResult{Stdout: "ok", ExitCode: 0}, nil
}

func (f *fakeAPI) ListCRs(_ context.Context, _, _, _, ns, sel string) ([]map[string]any, error) {
	var out []map[string]any
	for key, obj := range f.crs {
		if !strings.HasPrefix(key, ns+"/") {
			continue
		}
		match := true
		for _, kv := range strings.Split(sel, ",") {
			k, v, _ := strings.Cut(kv, "=")
			if digStr(obj, "metadata", "labels", k) != v {
				match = false
			}
		}
		if match {
			out = append(out, obj)
		}
	}
	return out, nil
}

func testConfig() Config {
	return Config{
		Kubeconfig: "x",
		Templates: map[string]Template{
			"python":  {Image: "python:3.12-slim", CPU: 1, MemoryGiB: 2},
			"pytorch": {Image: "pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime", CPU: 4, MemoryGiB: 16},
		},
		GPUModels: []string{"nvidia-3090"},
		Limits:    Limits{MaxConcurrent: 2, MaxConcurrentGPUs: 1},
	}
}

func newTestService(f *fakeAPI) *Service {
	s := NewWithAPI(f, testConfig(), "svc-sbx")
	s.now = func() time.Time { return time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC) }
	return s
}

func TestConfigValidateAndDefaults(t *testing.T) {
	if err := (Config{}).Validate(); err == nil || !strings.Contains(err.Error(), "kubeconfig") || !strings.Contains(err.Error(), "templates") {
		t.Errorf("empty config: %v", err)
	}
	bad := testConfig()
	bad.Templates["Bad_Name"] = Template{}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "Bad_Name") {
		t.Errorf("bad template: %v", err)
	}
	if err := testConfig().Validate(); err != nil {
		t.Errorf("good config: %v", err)
	}
	c := testConfig().WithDefaults()
	if c.RuntimeClass != "gvisor" || c.Limits.MaxCPU != 8 || c.Limits.DefaultTimeoutSeconds != 3600 || c.Limits.MaxConcurrent != 2 {
		t.Errorf("defaults: %+v", c)
	}
}

func TestSpecValidate(t *testing.T) {
	cfg := testConfig()
	cases := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{"unknown template", Spec{Template: "nope"}, "unknown template"},
		{"too much cpu", Spec{Template: "python", CPU: 99}, "cpu must be"},
		{"unknown gpu", Spec{Template: "pytorch", GPU: &GPURequest{Model: "h100"}}, "unknown GPU model"},
		{"too many gpus", Spec{Template: "pytorch", GPU: &GPURequest{Model: "nvidia-3090", Count: 2}}, "gpu.count"},
		{"timeout too short", Spec{Template: "python", TimeoutSeconds: -1}, "timeoutSeconds"},
		{"bad env", Spec{Template: "python", Env: map[string]string{"1BAD": "x"}}, "invalid environment"},
		{"ok", Spec{Template: "python"}, ""},
	}
	for _, c := range cases {
		err := c.spec.Validate(cfg)
		if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErr)
		}
	}
	s := Spec{Template: "pytorch", GPU: &GPURequest{Model: "nvidia-3090"}}
	if err := s.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if s.CPU != 4 || s.MemoryGiB != 16 || s.GPU.Count != 1 || s.TimeoutSeconds != 3600 {
		t.Errorf("defaults not filled: %+v", s)
	}
}

func TestBuildCRSecurityPosture(t *testing.T) {
	s := Spec{ID: "sbx-1", ProjectID: "p1", Template: "pytorch", CPU: 2, MemoryGiB: 8, GPU: &GPURequest{Model: "nvidia-3090", Count: 1}, Env: map[string]string{"TOKEN": "secret"}}
	cr := BuildCR(testConfig(), "svc-sbx", s, time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC))
	pod := dig(cr, "spec", "podTemplate", "spec").(map[string]any)
	if pod["runtimeClassName"] != "gvisor" {
		t.Errorf("runtimeClassName = %v", pod["runtimeClassName"])
	}
	if pod["automountServiceAccountToken"] != false || pod["enableServiceLinks"] != false {
		t.Error("service account token / service links must be off")
	}
	if digStr(cr, "metadata", "namespace") != "sb-p1" || digStr(cr, "spec", "shutdownTime") != "2026-11-05T11:00:00Z" || digStr(cr, "spec", "shutdownPolicy") != "Delete" {
		t.Errorf("metadata/shutdown wrong: %v", cr)
	}
	if digStr(pod, "nodeSelector", LabelGPU) != "nvidia-3090" {
		t.Errorf("gpu node selector missing: %v", pod["nodeSelector"])
	}
	c := firstContainer(cr)
	req, lim := dig(c, "resources", "requests").(map[string]any), dig(c, "resources", "limits").(map[string]any)
	if req[ResourceGPU] != "1" || lim["cpu"] != "2" || lim["memory"] != "8Gi" || req["cpu"] != lim["cpu"] {
		t.Errorf("resources: req=%v lim=%v", req, lim)
	}
	if c["command"].([]any)[0] != "sleep" {
		t.Errorf("default command: %v", c["command"])
	}
	// CPU sandbox: no selector, no toleration, no GPU resource.
	cpu := BuildCR(testConfig(), "svc-sbx", Spec{ID: "sbx-2", ProjectID: "p1", Template: "python", CPU: 1, MemoryGiB: 2}, time.Now())
	cpod := dig(cpu, "spec", "podTemplate", "spec").(map[string]any)
	if cpod["nodeSelector"] != nil || cpod["tolerations"] != nil || dig(firstContainer(cpu), "resources", "limits", ResourceGPU) != nil {
		t.Errorf("cpu sandbox must not touch GPU nodes: %v", cpod)
	}
}

func gpuNode(name, model string, gpus int, ready bool) map[string]any {
	status := "False"
	if ready {
		status = "True"
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "labels": map[string]any{LabelGPU: model}},
		"spec":     map[string]any{"taints": []any{map[string]any{"key": ResourceGPU, "effect": "NoSchedule"}}},
		"status": map[string]any{"allocatable": map[string]any{ResourceGPU: strconv.Itoa(gpus)},
			"conditions": []any{map[string]any{"type": "Ready", "status": status}}},
	}
}

func TestCreateEnforcesQuotaAndNamespace(t *testing.T) {
	f := newFake()
	f.nodes = []map[string]any{gpuNode("g1", "nvidia-3090", 1, true)}
	s := newTestService(f)
	ctx := context.Background()

	gpu := Spec{ProjectID: "p1", Template: "pytorch", GPU: &GPURequest{Model: "nvidia-3090"}}
	data, err := s.CreateSandbox(ctx, &gpu)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gpu.ID, "sbx-") || data["status"] != StatusPending || data["gpuCount"] != 1 {
		t.Errorf("create: id=%s data=%v", gpu.ID, data)
	}
	if f.namespaces["sb-p1"][LabelTenant] != "true" {
		t.Error("namespace must carry the tenant label (egress guardrail)")
	}
	if digStr(f.quotas["sb-p1"], "spec", "hard", "requests."+ResourceGPU) != "1" {
		t.Errorf("quota: %v", f.quotas["sb-p1"])
	}

	// A second GPU exceeds MaxConcurrentGPUs=1.
	gpu2 := Spec{ProjectID: "p1", Template: "pytorch", GPU: &GPURequest{Model: "nvidia-3090"}}
	if _, err := s.CreateSandbox(ctx, &gpu2); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("second GPU: err = %v, want quota", err)
	}
	// One CPU sandbox fits (2 total), a third does not.
	cpu := Spec{ProjectID: "p1", Template: "python"}
	if _, err := s.CreateSandbox(ctx, &cpu); err != nil {
		t.Fatal(err)
	}
	cpu2 := Spec{ProjectID: "p1", Template: "python"}
	if _, err := s.CreateSandbox(ctx, &cpu2); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("third sandbox: err = %v, want quota", err)
	}
	// Another project has its own budget.
	other := Spec{ProjectID: "p2", Template: "python"}
	if _, err := s.CreateSandbox(ctx, &other); err != nil {
		t.Errorf("other project: %v", err)
	}
}

func TestOwnershipTimeoutDeleteAndSecrets(t *testing.T) {
	f := newFake()
	s := newTestService(f)
	ctx := context.Background()
	sp := Spec{ProjectID: "p1", Template: "python", Env: map[string]string{"API_KEY": "s3cret"}}
	if _, err := s.CreateSandbox(ctx, &sp); err != nil {
		t.Fatal(err)
	}

	// The cache payload keeps env names, never values.
	data, err := s.GetSandbox(ctx, "p1", sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range data {
		if v == "s3cret" {
			t.Fatal("env value leaked into the cache payload")
		}
	}
	if names, _ := data["envNames"].([]any); len(names) != 1 || names[0] != "API_KEY" {
		t.Errorf("envNames = %v", data["envNames"])
	}

	// Another project cannot address it, even by guessing the id.
	if _, err := s.GetSandbox(ctx, "p2", sp.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-project get: %v", err)
	}
	// An object in the right namespace but owned by another service is not ours.
	f.crs["sb-p1/foreign"] = map[string]any{"metadata": map[string]any{"name": "foreign", "labels": map[string]any{
		LabelProject: "p1", LabelManagedBy: ManagedByValue, LabelService: "svc-other"}}}
	if _, err := s.GetSandbox(ctx, "p1", "foreign"); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign service get: %v", err)
	}
	if err := s.DeleteSandbox(ctx, "p1", "foreign"); err != nil || f.crs["sb-p1/foreign"] == nil {
		t.Errorf("delete of a foreign object must be a no-op: err=%v", err)
	}
	if list, _ := s.ListSandboxes(ctx, "p1"); len(list) != 1 {
		t.Errorf("list must only show owned sandboxes: %v", list)
	}

	// Timeout: bounded, and rewrites shutdownTime from now.
	if _, err := s.SetTimeout(ctx, "p1", sp.ID, 0); !errors.Is(err, ErrBadTimeout) {
		t.Errorf("short timeout: %v", err)
	}
	d, err := s.SetTimeout(ctx, "p1", sp.ID, 7200)
	if err != nil || d["shutdownTime"] != "2026-11-05T12:00:00Z" {
		t.Errorf("SetTimeout = %v, %v", d, err)
	}

	if err := s.DeleteSandbox(ctx, "p1", sp.ID); err != nil || f.crs["sb-p1/"+sp.ID] != nil {
		t.Errorf("delete: %v", err)
	}
	if err := s.DeleteSandbox(ctx, "p1", sp.ID); err != nil {
		t.Errorf("idempotent delete: %v", err)
	}
	if err := s.DeleteProjectNamespace(ctx, "p1"); err != nil || len(f.deletedNS) != 1 {
		t.Errorf("namespace delete: %v %v", err, f.deletedNS)
	}
}

func TestSyncProviderList(t *testing.T) {
	f := newFake()
	s := newTestService(f)
	ctx := context.Background()
	sp := Spec{ProjectID: "p1", Template: "python"}
	if _, err := s.CreateSandbox(ctx, &sp); err != nil {
		t.Fatal(err)
	}
	f.crs["sb-p1/"+sp.ID]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	terminating := Spec{ProjectID: "p1", Template: "python"}
	if _, err := s.CreateSandbox(ctx, &terminating); err != nil {
		t.Fatal(err)
	}
	f.crs["sb-p1/"+terminating.ID]["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-05T10:00:00Z"

	rows, err := s.SyncProvider("az1", "p1").List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ExternalID != sp.ID || rows[0].Data["status"] != StatusRunning || rows[0].Region != "az1" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestExecGuards(t *testing.T) {
	f := newFake()
	s := newTestService(f)
	ctx := context.Background()
	sp := Spec{ProjectID: "p1", Template: "python"}
	if _, err := s.CreateSandbox(ctx, &sp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(ctx, "p1", sp.ID, []string{"true"}, nil, 0); !errors.Is(err, ErrNotRunning) {
		t.Errorf("exec before Ready: %v", err)
	}
	f.crs["sb-p1/"+sp.ID]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	if _, err := s.Exec(ctx, "p1", sp.ID, nil, nil, 0); !errors.Is(err, ErrBadExec) {
		t.Errorf("empty command: %v", err)
	}
	if _, err := s.Exec(ctx, "p1", sp.ID, []string{"x"}, nil, time.Hour); !errors.Is(err, ErrBadExec) {
		t.Errorf("timeout over max: %v", err)
	}
	if _, err := s.Exec(ctx, "p2", sp.ID, []string{"x"}, nil, 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-project exec: %v", err)
	}
	res, err := s.Exec(ctx, "p1", sp.ID, []string{"python", "-c", "print(1)"}, []byte("in"), 0)
	if err != nil || res.Stdout != "ok" {
		t.Fatalf("exec: %+v %v", res, err)
	}
	if want := "sb-p1/" + sp.ID + "/main:python -c print(1)<in"; len(f.execs) != 1 || f.execs[0] != want {
		t.Errorf("exec target = %v, want %s", f.execs, want)
	}
}

func TestBuiltinRuntimeEnv(t *testing.T) {
	cfg := Config{Envd: true, Templates: map[string]Template{"ci": {Image: "ci", CPU: 1, MemoryGiB: 1,
		StartCmd: "serve", ReadyCmd: "probe", User: "user", Workdir: "/home/user", StartAsRoot: true}}}
	cr := BuildCR(cfg, "svc", Spec{ID: "sbx-00000001", ProjectID: "p1", Template: "ci", CPU: 1, MemoryGiB: 1}, time.Unix(0, 0))
	env, _ := json.Marshal(firstContainer(cr)["env"])
	for _, want := range []string{`"PROXCLOUD_START_CMD","value":"serve"`, `"PROXCLOUD_READY_CMD","value":"probe"`, `"PROXCLOUD_DEFAULT_USER","value":"user"`, `"PROXCLOUD_START_AS_ROOT","value":"1"`} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env lacks %s: %s", want, env)
		}
	}
}
