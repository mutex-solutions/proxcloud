package project

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// fnTestKeys is a portal encryption key for the function tests.
func fnTestKeys(t testing.TB) *textcrypt.BlobKeys {
	t.Helper()
	keys, err := textcrypt.NewBlobKeys("k1", map[string]string{"k1": strings.Repeat("function-test-key-", 2)})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestFunctionStatus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fn    functionDoc
		ready int
		want  string
	}{
		{"first rollout", functionDoc{DeployingID: "d1"}, 0, fnDeploying},
		{"redeploy while serving", functionDoc{DeployingID: "d2", CurrentDeploymentID: "d1"}, 1, fnDeploying},
		{"serving", functionDoc{CurrentDeploymentID: "d1"}, 1, fnReady},
		{"scaled to zero", functionDoc{CurrentDeploymentID: "d1"}, 0, fnIdle},
		{"nothing healthy was ever deployed", functionDoc{LastError: "image pull failed"}, 0, fnFailed},
		{"a failed redeploy keeps serving the previous one", functionDoc{CurrentDeploymentID: "d1", LastError: "x"}, 1, fnReady},
		{"suspended wins", functionDoc{Suspended: true, DeployingID: "d2", CurrentDeploymentID: "d1"}, 1, fnSuspended},
	} {
		if got := functionStatus(&tc.fn, tc.ready); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The floor a function is kept at, and when its cloud row no longer says what billing must charge.
func TestFunctionFloorAndRowStale(t *testing.T) {
	fc := sandbox.FunctionsConfig{MinInstances: 1}
	if got := functionFloor(&functionDoc{CurrentDeploymentID: "d1"}, fc); got != 1 {
		t.Errorf("platform floor of one: %d", got)
	}
	if got := functionFloor(&functionDoc{}, fc); got != 0 {
		t.Errorf("no working deployment: floor %d, want 0", got)
	}
	// The function's own minInstances counts as far as the provider allows it (maxMinInstances).
	own := &functionDoc{CurrentDeploymentID: "d1"}
	own.MinInstances = 1
	if got := functionFloor(own, sandbox.FunctionsConfig{}); got != 0 {
		t.Errorf("minInstances 1 where the provider allows none: floor %d, want 0", got)
	}
	if got := functionFloor(own, sandbox.FunctionsConfig{MaxMinInstances: 1}); got != 1 {
		t.Errorf("minInstances 1 where the provider allows one: floor %d, want 1", got)
	}
	want := map[string]any{"instances": 1, "status": functionRowRunning, "cpuMillis": 250, "memoryMi": 256}
	same := map[string]any{"instances": float64(1), "status": functionRowRunning, "cpuMillis": float64(250), "memoryMi": float64(256), "name": "x"}
	if functionRowStale(same, want) {
		t.Error("a row read back from the store (numbers as float64) must not count as stale")
	}
	for k, v := range map[string]any{"instances": float64(0), "status": functionRowIdle, "cpuMillis": float64(2000), "memoryMi": float64(4096), "billedInstances": float64(3)} {
		row := map[string]any{"instances": float64(1), "status": functionRowRunning, "cpuMillis": float64(250), "memoryMi": float64(256)}
		row[k] = v
		if !functionRowStale(row, want) {
			t.Errorf("a row with another %s must count as stale", k)
		}
	}
}

// The intervals that ended, as kept at a stop: the one that ends is added with the count and size it is billed at,
// and those that ended more than functionEndedKeep ago are let go. The row carries them as billing reads them, each
// with its own size.
func TestFunctionEndedIntervals(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	since := ago(90 * time.Second)
	h := &Handler{}
	fn := &functionDoc{RunningSince: &since, LastInstances: 3, Ended: []fnEnded{
		{From: ago(30 * time.Minute), To: ago(functionEndedKeep + time.Second), Instances: 1, CPUMillis: 250, MemoryMi: 256},
		{From: ago(12 * time.Minute), To: ago(functionEndedKeep), Instances: 2, CPUMillis: 250, MemoryMi: 256},
	}}
	fn.CPUMillis, fn.MemoryMi = 1000, 2048 // deployed since the two ended
	want := []fnEnded{fn.Ended[1], {From: since, To: now, Instances: 3, CPUMillis: 1000, MemoryMi: 2048}}
	if got := h.endedIntervals(t.Context(), fn, now); !reflect.DeepEqual(got, want) || len(fn.Ended) != 2 || fn.Ended[0].Instances != 1 {
		t.Errorf("ended intervals = %+v (record %+v), want %+v", got, fn.Ended, want)
	}
	if got := h.endedIntervals(t.Context(), &functionDoc{RunningSince: &since}, now); len(got) != 1 || got[0].Instances != 1 {
		t.Errorf("an interval with no recorded count is billed at one: %+v", got)
	}
	if got := h.endedIntervals(t.Context(), &functionDoc{}, now); len(got) != 0 {
		t.Errorf("a function that never ran has no interval: %+v", got)
	}
	fn.Ended, fn.StoppedAt = want, &now
	row := h.functionRowData(t.Context(), fn)
	if got, _ := json.Marshal(row["endedIntervals"]); string(got) != `[{"cpuMillis":250,"from":"2026-11-16T11:48:00Z","instances":2,"memoryMi":256,"to":"2026-11-16T11:50:00Z"},`+
		`{"cpuMillis":1000,"from":"2026-11-16T11:58:30Z","instances":3,"memoryMi":2048,"to":"2026-11-16T12:00:00Z"}]` ||
		row["runningSince"] != "2026-11-16T11:58:30Z" || row["stoppedAt"] != "2026-11-16T12:00:00Z" {
		t.Errorf("row = %s, runningSince %v, stoppedAt %v", got, row["runningSince"], row["stoppedAt"])
	}
	// Once stopped the row is billed at the count it last ran with, never at zero.
	if row["instances"] != 0 || row["billedInstances"] != 3 || row["status"] != functionRowIdle {
		t.Errorf("a stopped function's row: %v", row)
	}
	if _, has := (&Handler{}).functionRowData(t.Context(), &functionDoc{})["endedIntervals"]; has {
		t.Error("a row without ended intervals must not carry the key")
	}
}

// The row is what the generic resource list shows and what billing reads: it names the function's secrets and holds
// nothing else of them, nor anything of its registry credentials.
func TestFunctionRowHoldsNoSealedValue(t *testing.T) {
	fn := &functionDoc{ID: "fn-0123456789abcdef", Host: "api-a1b2c3.fn.example",
		Secrets:          map[string]fnSecret{"TOKEN": {KeyID: "k1", Sealed: "c2VhbGVkLXRva2Vu"}},
		RegistryPassword: &fnSecret{KeyID: "k1", Sealed: "c2VhbGVkLXBhc3N3b3Jk"}}
	fn.Name, fn.Image, fn.SecretNames = "api", "nginx:1", []string{"TOKEN"}
	fn.RegistryAuth = &function.RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v1"}
	row, _ := json.Marshal((&Handler{}).functionRowData(t.Context(), fn))
	for _, no := range []string{"c2VhbGVk", "sealed", "keyId", "bot", "ghcr.io"} {
		if strings.Contains(string(row), no) {
			t.Errorf("the row carries %q: %s", no, row)
		}
	}
}

// Keep the 10 newest deployment records, and always the one that is serving.
func TestPrunableDeployments(t *testing.T) {
	var deps []functionDeployment
	for seq := 14; seq >= 1; seq-- { // newest first
		deps = append(deps, functionDeployment{ID: fmt.Sprint("d", seq), Seq: seq})
	}
	if got := prunableDeployments(deps[:10], "d5"); got != nil {
		t.Errorf("10 records: pruned %v, want none", got)
	}
	if got, want := prunableDeployments(deps, "d14"), []string{"d4", "d3", "d2", "d1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned %v, want %v", got, want)
	}
	// Ten failed deployments after the serving one: it is older than the newest ten and is kept.
	if got, want := prunableDeployments(deps, "d3"), []string{"d4", "d2", "d1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pruned %v, want %v (the serving deployment stays)", got, want)
	}
}

func TestFunctionHTTPError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{errSandboxForbidden("the billing profile is suspended"), http.StatusForbidden},
		{function.ErrNotFound, http.StatusNotFound},
		{fmt.Errorf("wrapped: %w", function.ErrDisabled), http.StatusForbidden},
		{errFunctionBusy, http.StatusConflict},
		{errFunctionQuota("the project already has 3 functions, the most it may have"), http.StatusTooManyRequests},
		{kamajik8s.ErrUnreachable, http.StatusServiceUnavailable},
		{&kamajik8s.APIError{Status: 403, Message: `pods "x" is forbidden: exceeded quota: proxcloud-function`}, http.StatusTooManyRequests},
		{&kamajik8s.APIError{Status: 422, Message: "Deployment.apps is invalid: node worker-1"}, http.StatusBadRequest},
		{&kamajik8s.APIError{Status: 403, Message: "forbidden: User system:serviceaccount:x"}, http.StatusServiceUnavailable},
		{&kamajik8s.APIError{Status: 500}, http.StatusServiceUnavailable},
	} {
		status, msg, ok := functionHTTPError(tc.err)
		if !ok || status != tc.want {
			t.Errorf("%v: %d (mapped %v), want %d", tc.err, status, ok, tc.want)
		}
		// Kubernetes' own words (namespaces, service accounts, nodes) never reach the caller.
		var kerr *kamajik8s.APIError
		if errors.As(tc.err, &kerr) && kerr.Message != "" && (msg == kerr.Message || len(msg) > 80) {
			t.Errorf("%v: message %q echoes the cluster", tc.err, msg)
		}
	}
	if _, _, ok := functionHTTPError(errors.New("db down")); ok {
		t.Error("an unknown error must fall through to the generic 500")
	}
}

func TestFunctionSnapshotAndRow(t *testing.T) {
	a := function.Snapshot{Image: "nginx:1", Port: 8080, CPUMillis: 250, MemoryMi: 256}
	b := a
	b.Env, b.Command = map[string]string{}, []string{} // empty and absent are the same config
	if !sameSnapshot(a, b) {
		t.Error("empty env/command must not count as a change")
	}
	for name, change := range map[string]func(*function.Snapshot){
		"image":   func(s *function.Snapshot) { s.Image = "nginx:2" },
		"port":    func(s *function.Snapshot) { s.Port = 9000 },
		"cpu":     func(s *function.Snapshot) { s.CPUMillis = 500 },
		"memory":  func(s *function.Snapshot) { s.MemoryMi = 512 },
		"env":     func(s *function.Snapshot) { s.Env = map[string]string{"A": "1"} },
		"command": func(s *function.Snapshot) { s.Command = []string{"serve"} },
		"args":    func(s *function.Snapshot) { s.Args = []string{"-v"} },
		"secrets": func(s *function.Snapshot) { s.SecretsVersion = "v2" },
		"registry": func(s *function.Snapshot) {
			s.RegistryAuth = &function.RegistryAuth{Server: "ghcr.io", Username: "bot", Version: "v1"}
		},
	} {
		c := a
		change(&c)
		if sameSnapshot(a, c) {
			t.Errorf("a changed %s must make a new deployment", name)
		}
	}

	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	running := map[string]any{"name": "api", "instances": float64(1), "billedInstances": float64(1), "status": functionRowRunning, "runningSince": "2026-11-16T11:00:00Z"}
	d := deletedFunctionRow(running, now)
	if d["status"] != functionRowDeleted || d["instances"] != 0 || d["stoppedAt"] != "2026-11-16T12:00:00Z" || d["billedInstances"] != float64(1) || running["status"] != functionRowRunning {
		t.Errorf("deleted while running: %v (source %v)", d, running)
	}
	// Deleted while already stopped: its end stays where it was (billing must not extend it).
	idle := map[string]any{"instances": float64(0), "status": functionRowIdle, "stoppedAt": "2026-11-16T11:30:00Z"}
	if d := deletedFunctionRow(idle, now); d["stoppedAt"] != "2026-11-16T11:30:00Z" || d["status"] != functionRowDeleted {
		t.Errorf("deleted while stopped: %v", d)
	}

	// env values are for those who may change the function; others see the names.
	env := map[string]string{"TOKEN": "s3cret"}
	if got := (&fnCaller{}).env(env); got["TOKEN"] != "" || len(got) != 1 || env["TOKEN"] != "s3cret" {
		t.Errorf("read-only env = %v (source %v)", got, env)
	}
	if got := (&fnCaller{manage: true}).env(env); got["TOKEN"] != "s3cret" {
		t.Errorf("manager env = %v", got)
	}
}

// Registry credentials are set with their password, every time, and get a new version each time (the same password
// is a change too). What is shown of them is the server, the user and when they were set: the function's view has
// the place of the snapshot's field, version included.
func TestRegistryAuthRequestAndView(t *testing.T) {
	for _, password := range []string{"", strings.Repeat("x", maxRegistryPassword+1)} {
		if _, err := (&registryAuthReq{Username: "bot", Password: password}).describe(); err == nil || !strings.Contains(err.Error(), "registryAuth.password is required") {
			t.Errorf("a password of %d bytes: %v", len(password), err)
		}
	}
	req := &registryAuthReq{Server: " ghcr.io ", Username: "bot", Password: "hunter2"}
	a, _ := req.describe()
	b, err := req.describe()
	if err != nil || a.Server != "ghcr.io" || a.Username != "bot" || a.Version == "" || a.Version == b.Version {
		t.Fatalf("describe: %+v, %+v (%v)", a, b, err)
	}
	if registryVersion(nil) != "" || registryVersion(a) != a.Version {
		t.Error("registryVersion")
	}

	// A request keeps (not given), removes (null) or sets the credentials of the snapshot it rolls out. Setting them
	// takes the portal's encryption key: without one the password would be stored as sent.
	h := &Handler{fnKeys: fnTestKeys(t)}
	fn := &functionDoc{ID: "fn-0123456789abcdef", ProjectID: "p", Host: "api-a1b2c3.fn.example"}
	snap := function.Snapshot{RegistryAuth: a}
	if p, err := h.registryAuthChange(fn, &snap, false, nil); p != nil || err != nil || snap.RegistryAuth != a {
		t.Errorf("not given: %v %v %+v", p, err, snap.RegistryAuth)
	}
	p, err := h.registryAuthChange(fn, &snap, true, req)
	if err != nil || p == nil || strings.Contains(p.Sealed, "hunter2") || p.KeyID != "k1" || snap.RegistryAuth == a || snap.RegistryAuth.Username != "bot" {
		t.Fatalf("set: %+v %v %+v", p, err, snap.RegistryAuth)
	}
	if raw, _ := base64.StdEncoding.DecodeString(p.Sealed); strings.Contains(string(raw), "hunter2") {
		t.Fatal("the stored password is not sealed")
	}
	if got, err := h.openValue(fn, fnRegistryPasswordName, *p); err != nil || got != "hunter2" {
		t.Fatalf("the sealed password opens as %q (%v)", got, err)
	}
	var herr *httpx.HTTPError
	if p, err := (&Handler{}).registryAuthChange(fn, &snap, true, req); p != nil || !errors.As(err, &herr) || herr.Status != http.StatusServiceUnavailable || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("set without an encryption key: %+v %v", p, err)
	}
	if p, err := h.registryAuthChange(fn, &snap, true, &registryAuthReq{Username: "bot"}); p != nil || !errors.As(err, &herr) || herr.Status != http.StatusBadRequest {
		t.Errorf("set without a password: %+v %v", p, err)
	}
	if p, err := h.registryAuthChange(fn, &snap, true, nil); p != nil || err != nil || snap.RegistryAuth != nil {
		t.Errorf("null: %v %v %+v", p, err, snap.RegistryAuth)
	}

	set := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	fn.RegistryPassword = &fnSecret{KeyID: "k1", Sealed: "c2VhbGVkLWh1bnRlcjI=", SetAt: set}
	fn.Name, fn.Image, fn.RegistryAuth = "api", "ghcr.io/acme/api:1", a
	out, _ := json.Marshal((&fnCaller{manage: true}).view(fn, 0))
	if !strings.Contains(string(out), `"registryAuth":{"server":"ghcr.io","username":"bot","setAt":"2026-11-16T12:00:00Z"}`) ||
		strings.Contains(string(out), "hunter2") || strings.Contains(string(out), a.Version) || strings.Contains(string(out), "assword") ||
		strings.Contains(string(out), "c2VhbGVk") || strings.Contains(string(out), "keyId") {
		t.Errorf("view: %s", out)
	}
	fn.RegistryAuth, fn.RegistryPassword = nil, nil
	if out, _ = json.Marshal((&fnCaller{}).view(fn, 0)); strings.Contains(string(out), "registryAuth") {
		t.Errorf("view without credentials: %s", out)
	}
}

// A request's secrets are merged into the function's: a string sets, null removes, the rest stays. A set name always
// counts as a change, also to the same value. The stored form of the merge is sealed and keeps the records of the
// names that stay.
func TestMergeSecrets(t *testing.T) {
	str := func(s string) *string { return &s }
	cur := map[string]string{"A": "one", "B": "two"}
	if got, changed := mergeSecrets(nil, map[string]*string{"A": str("one"), "B": str("two"), "GONE": nil}); !changed || !reflect.DeepEqual(got, cur) {
		t.Fatalf("first set: %v %v", got, changed)
	}
	if got, changed := mergeSecrets(cur, map[string]*string{"A": nil, "C": str("three")}); !changed || !reflect.DeepEqual(got, map[string]string{"B": "two", "C": "three"}) || len(cur) != 2 {
		t.Fatalf("merge: %v %v (source %v)", got, changed, cur)
	}
	for name, given := range map[string]map[string]*string{"nothing": nil, "empty": {}, "removing what is not there": {"NOPE": nil}} {
		if got, changed := mergeSecrets(cur, given); changed || !reflect.DeepEqual(got, cur) {
			t.Errorf("%s: changed=%v %v", name, changed, got)
		}
	}
	if _, changed := mergeSecrets(cur, map[string]*string{"A": str("one")}); !changed {
		t.Error("the same value again must still be a change")
	}

	h := &Handler{fnKeys: fnTestKeys(t)}
	fn := &functionDoc{ID: "fn-0123456789abcdef", ProjectID: "p"}
	t0, t1 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC), time.Date(2026, 11, 16, 13, 0, 0, 0, time.UTC)
	set, err := h.sealSecrets(fn, map[string]*string{"A": str("one"), "B": str("two"), "GONE": nil}, t0)
	if err != nil || len(set) != 2 || set["A"].Sealed == "one" || set["A"].KeyID != "k1" || set["A"].SetAt != t0 {
		t.Fatalf("sealed: %v (%v)", set, err)
	}
	if got, err := h.openSecrets(fn, set); err != nil || !reflect.DeepEqual(got, cur) {
		t.Fatalf("the sealed set opens as %v (%v)", got, err)
	}
	fn.Secrets = set
	next, err := h.sealSecrets(fn, map[string]*string{"A": nil, "C": str("three")}, t1)
	if err != nil || len(next) != 2 || next["B"] != set["B"] || next["C"].SetAt != t1 || len(set) != 2 {
		t.Fatalf("sealed merge: %v (source %v, %v)", next, set, err)
	}
	if got, err := h.openSecrets(fn, next); err != nil || !reflect.DeepEqual(got, map[string]string{"B": "two", "C": "three"}) {
		t.Fatalf("the merged set opens as %v (%v)", got, err)
	}
	// Without the portal's key nothing is sealed, so nothing can be stored.
	if set, err := (&Handler{}).sealSecrets(&functionDoc{ID: fn.ID, ProjectID: "p"}, map[string]*string{"A": str("one")}, t0); set != nil || !errors.Is(err, errNoFunctionKey) {
		t.Errorf("sealing without a key: %v %v", set, err)
	}

	// What a snapshot knows of a set: sorted names and a version, new each time; nothing for none.
	var a, b function.Snapshot
	describeSecrets(&a, cur)
	describeSecrets(&b, cur)
	if !reflect.DeepEqual(a.SecretNames, []string{"A", "B"}) || a.SecretsVersion == "" || a.SecretsVersion == b.SecretsVersion {
		t.Errorf("describe: %+v %+v", a, b)
	}
	if describeSecrets(&a, nil); a.SecretNames != nil || a.SecretsVersion != "" {
		t.Errorf("describe no secrets: %+v", a)
	}
}

// A sealed value opens only as what it was sealed for: this project, this function, this name. A blob copied to
// another function's record, to another name of the same function, to another project, or into the place of the
// registry password opens as nothing; so does one sealed under a key the portal no longer has.
func TestFunctionSealedValueIsBoundToItsPlace(t *testing.T) {
	h := &Handler{fnKeys: fnTestKeys(t)}
	fn := &functionDoc{ID: "fn-0123456789abcdef", ProjectID: "p"}
	sealed, err := h.sealValue(fn, "TOKEN", "hunter2", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := h.openValue(fn, "TOKEN", sealed); err != nil || got != "hunter2" {
		t.Fatalf("in its own place: %q %v", got, err)
	}
	other := func(change func(*functionDoc)) *functionDoc {
		c := *fn
		change(&c)
		return &c
	}
	for name, tc := range map[string]struct {
		fn   *functionDoc
		name string
	}{
		"another function":       {other(func(f *functionDoc) { f.ID = "fn-fedcba9876543210" }), "TOKEN"},
		"another project":        {other(func(f *functionDoc) { f.ProjectID = "q" }), "TOKEN"},
		"another name":           {fn, "OTHER"},
		"the registry password":  {fn, fnRegistryPasswordName},
		"a name that joins both": {other(func(f *functionDoc) { f.ID = "fn-0123456789abcdef/TOKEN" }), ""},
	} {
		if got, err := h.openValue(tc.fn, tc.name, sealed); got != "" || !errors.Is(err, errFunctionSealed) {
			t.Errorf("%s: opened as %q (%v)", name, got, err)
		}
	}
	rotated, err := textcrypt.NewBlobKeys("k2", map[string]string{"k2": strings.Repeat("another-portal-key-", 2)})
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]*Handler{"no key": {}, "another key": {fnKeys: rotated}} {
		if got, err := h.openValue(fn, "TOKEN", sealed); got != "" || !errors.Is(err, errFunctionSealed) {
			t.Errorf("%s: opened as %q (%v)", name, got, err)
		}
	}
	if got, err := h.openValue(fn, "TOKEN", fnSecret{KeyID: "k1", Sealed: "not base64!"}); got != "" || !errors.Is(err, errFunctionSealed) {
		t.Errorf("a blob that is no blob: %q (%v)", got, err)
	}
	// The registry password's name is none a secret can have.
	if function.ValidateSecrets(nil, map[string]string{fnRegistryPasswordName: "x"}) == nil {
		t.Error("the name the registry password is sealed as must not be a valid secret name")
	}
}

// A rollback to a deployment that ran a tag deploys the digest it ran. A reference by digest stays as it is, and so
// does one whose digest the runtime never told.
func TestRollbackImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct{ image, digest, want string }{
		{"nginx:1", digest, "nginx@" + digest},
		{"nginx", digest, "nginx@" + digest},
		{"registry.example:5000/acme/api:1.2", digest, "registry.example:5000/acme/api@" + digest},
		{"registry.example:5000/acme/api", digest, "registry.example:5000/acme/api@" + digest},
		{"ghcr.io/acme/api@sha256:" + strings.Repeat("b", 64), digest, "ghcr.io/acme/api@sha256:" + strings.Repeat("b", 64)},
		{"nginx:1", "", "nginx:1"},
	} {
		dep := &functionDeployment{ImageDigest: tc.digest}
		dep.Image = tc.image
		if got := rollbackImage(dep); got != tc.want {
			t.Errorf("rollback to %s (ran %s): %s, want %s", tc.image, tc.digest, got, tc.want)
		}
	}
}

// What is not offered here is refused by name, not taken and ignored: the decoder is not strict.
func TestFunctionNotOfferedKeys(t *testing.T) {
	for body, refused := range map[string]bool{
		`{"domains":["shop.example.com"]}`:   true,
		`{"publicRateLimit":{"rps":10}}`:     true,
		`{"publicRateLimit":5}`:              true,
		`{"domains":null}`:                   false,
		`{"name":"api","image":"nginx:1"}`:   false,
		`{"env":{"domains":"a value only"}}`: false,
	} {
		var given map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &given); err != nil {
			t.Fatal(err)
		}
		err := notOffered(given)
		var herr *httpx.HTTPError
		if refused != (errors.As(err, &herr) && herr.Status == http.StatusBadRequest && strings.Contains(herr.Msg, "not offered")) {
			t.Errorf("%s: %v, refused want %v", body, err, refused)
		}
	}
	// gpu, source and a public exposure are refused by the spec itself, on a create and on an update alike.
	for _, body := range []string{`{"gpu":{"model":"nvidia-3090","count":1}}`, `{"source":{"repo":"acme/api"}}`, `{"exposure":"public"}`} {
		spec := function.Spec{Name: "api"}
		spec.Image = "nginx:1"
		if err := json.Unmarshal([]byte(body), &spec); err != nil {
			t.Fatal(err)
		}
		if err := spec.Validate(sandbox.FunctionsConfig{}); !errors.Is(err, function.ErrNotOffered) {
			t.Errorf("%s: %v, want not offered", body, err)
		}
	}
}

// What a function holds of its project's quota is reserved: every instance it may run, at its size.
func TestFunctionHold(t *testing.T) {
	for _, tc := range []struct {
		hold        fnHold
		cpu, memory int
	}{
		{fnHold{CPUMillis: 250, MemoryMi: 256, Instances: 1}, 250, 256},
		{fnHold{CPUMillis: 500, MemoryMi: 512, Instances: 4}, 2000, 2048},
		{fnHold{CPUMillis: 250, MemoryMi: 256}, 250, 256}, // a record with no maxInstances runs one
	} {
		if tc.hold.cpu() != tc.cpu || tc.hold.memory() != tc.memory {
			t.Errorf("%+v holds %d m / %d Mi, want %d / %d", tc.hold, tc.hold.cpu(), tc.hold.memory(), tc.cpu, tc.memory)
		}
	}
}

// The function routes of this release are registered, and nothing of the later ones is: no hostnames, no builds, no
// build log, no repository links. The two routes sandbox-proxy calls are registered beside the sandbox wake, in the
// main router and under no other mount.
func TestFunctionRoutes(t *testing.T) {
	api := chi.NewRouter()
	(&Handler{}).Routes(api)
	const fn = "/project/p1/functions/fn-0123456789abcdef"
	match := func(method, path string) bool { return api.Match(chi.NewRouteContext(), method, path) }
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/project/p1/functions"},
		{http.MethodPost, "/project/p1/functions"},
		{http.MethodGet, fn},
		{http.MethodPatch, fn},
		{http.MethodDelete, fn},
		{http.MethodGet, fn + "/deployments"},
		{http.MethodPost, fn + "/rollback"},
		{http.MethodPost, fn + "/action"},
		{http.MethodGet, fn + "/logs"},
		{http.MethodPost, "/internal/sandboxes/sbx-0123456789abcdef/wake"},
		{http.MethodPost, "/internal/functions/fn-0123456789abcdef/wake"},
		{http.MethodPost, "/internal/functions/activity"},
	} {
		if !match(tc.method, tc.path) {
			t.Errorf("no route for %s %s", tc.method, tc.path)
		}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, fn + "/builds"},
		{http.MethodPost, fn + "/domains"},
		{http.MethodDelete, fn + "/domains/shop.apps.example.com"},
		{http.MethodGet, fn + "/deployments/0123456789abcdef01234567/build-log"},
		{http.MethodGet, "/project/p1/github/install-url"},
		{http.MethodPost, "/github/callback"},
		{http.MethodGet, "/project/p1/github/installations"},
		{http.MethodDelete, "/project/p1/github/installations/42"},
		{http.MethodPost, "/project/p1/github/installations/42/refresh"},
		{http.MethodGet, "/project/p1/github/installations/42/repos"},
		{http.MethodPut, fn},
		{http.MethodPost, fn + "/deployments"},
		{http.MethodGet, "/internal/functions/activity"},
		{http.MethodPost, "/e2b/internal/functions/activity"},
		{http.MethodPost, "/internal/functions/fn-0123456789abcdef/sleep"},
	} {
		if match(tc.method, tc.path) {
			t.Errorf("%s %s is registered: it is not offered in this release", tc.method, tc.path)
		}
	}
}

// The service a project's console reads carries the functions block only where the location offers functions: the
// menu entry and the page hide together with the routes. The block has the limits the form checks against.
func TestFunctionsInTheServiceDTO(t *testing.T) {
	es := func(status string, functions map[string]any, domain string) *externalservice.ExternalService {
		return &externalservice.ExternalService{ID: "svc", Type: "CLOUD", Status: status, Config: map[string]any{
			"provider": "sandbox", "sandbox": map[string]any{"domain": domain, "functions": functions}}}
	}
	on := map[string]any{"enabled": true, "maxMinInstances": 1, "maxInstances": 3}
	dto := externalServiceDto(es(externalservice.StatusPublic, on, "sbx.example.org"))
	block, _ := dto["functions"].(map[string]any)
	if block == nil || block["domain"] != "sbx.example.org" || block["maxMinInstances"] != 1 || block["maxInstances"] != 3 ||
		block["idleSeconds"] != 300 || block["maxFunctions"] != 20 || block["defaultConcurrency"] != function.DefaultConcurrency {
		t.Fatalf("functions block of a location that offers them: %v", dto["functions"])
	}
	for name, off := range map[string]*externalservice.ExternalService{
		"the block is off":           es(externalservice.StatusPublic, map[string]any{"enabled": false, "maxInstances": 3}, "sbx.example.org"),
		"no block":                   es(externalservice.StatusPublic, nil, "sbx.example.org"),
		"a block that is not valid":  es(externalservice.StatusPublic, map[string]any{"enabled": true, "maxInstances": 1000}, "sbx.example.org"),
		"no domain to serve under":   es(externalservice.StatusPublic, on, ""),
		"the provider is disabled":   es(externalservice.StatusDisabled, on, "sbx.example.org"),
		"a provider of another kind": {ID: "pve", Type: "CLOUD", Config: map[string]any{"provider": "proxmox", "sandbox": map[string]any{"functions": on}}},
	} {
		if _, has := externalServiceDto(off)["functions"]; has {
			t.Errorf("%s: the service carries a functions block", name)
		}
	}
}
