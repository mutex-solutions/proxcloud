//go:build integration

package project

// The rules of scale to zero, wake and the two jobs, each with the test that fails without it: the wake route's
// answers and its token, the activity report's reach, when and how the reaper ends an interval, what the deploy sync
// does with a start that serves nothing and with a cluster that is gone. On real PostgreSQL with the cluster faked
// (fnEnv).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// otherProvider adds a second sandbox provider with functions on and a token of its own, and one function of
// project "foreign" recorded on it, running one instance. It returns the function's id.
func (e *fnEnv) otherProvider(token string) string {
	e.t.Helper()
	doc := &externalservice.ExternalService{ID: "svc2", Type: "CLOUD", Status: externalservice.StatusPublic,
		Config: map[string]any{"provider": "sandbox", "region": "az2", "sandbox": map[string]any{"domain": "fn2.example", "functions": map[string]any{"enabled": true, "maxInstances": 5}}},
		Secret: e.h.esSvc.EncryptSecret(map[string]any{"functionWakeToken": token})}
	if err := e.db.C("externalService").Upsert(e.ctx, "svc2", doc); err != nil {
		e.t.Fatal(err)
	}
	e.h.ForgetFunctionTokens()
	now := time.Now().UTC().Add(-time.Minute)
	fn := functionDoc{ID: "fn-00000000000000f2", ProjectID: "foreign", ServiceID: "svc2", Region: "az2", Host: "other-abcdef.fn2.example",
		CurrentDeploymentID: "d1", CurrentSeq: 1, Instances: 1, LastInstances: 1, RunningSince: &now, ReadyAt: &now, LastActiveAt: &now}
	fn.Name, fn.Image, fn.Port, fn.CPUMillis, fn.MemoryMi, fn.IdleSeconds, fn.MaxInstances, fn.Concurrency = "other", "nginx:1", 8080, 250, 256, 300, 5, 10
	if _, err := e.h.fns.InsertOne(e.ctx, &fn); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.h.syncFunctionRow(e.ctx, &fn, now); err != nil {
		e.t.Fatal(err)
	}
	return fn.ID
}

// envelope reads an answer of a proxy route: the portal's own, which is what sandbox-proxy's Waker reads.
func envelope(t *testing.T, body string) (data map[string]any, code float64, message string) {
	t.Helper()
	var env struct {
		Data   map[string]any `json:"data"`
		Errors struct {
			Code    float64 `json:"code"`
			Message string  `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("not the portal's envelope: %q (%v)", body, err)
	}
	return env.Data, env.Errors.Code, env.Errors.Message
}

// asleep brings a ready function to zero by the idle rule.
func (e *fnEnv) asleep(id string) {
	e.t.Helper()
	e.idle(id, time.Hour)
	e.reap()
	if fn := e.fn(id); fn.Instances != 0 || e.cluster.replicas("fn-"+fn.ProjectID, id) != 0 {
		e.t.Fatalf("function %s did not go to sleep: %+v", id, fn)
	}
}

// The wake route: the token is checked before anything about the function is looked up, and each state has its
// answer, in the portal's envelope. 200 starts exactly one instance and one interval; every refusal starts nothing.
func TestFunctionWakeRoute(t *testing.T) {
	e := newFnEnv(t)
	id := e.ready(`{"name":"api","image":"nginx:1"}`)
	e.asleep(id)
	other := e.otherProvider("token-of-svc2")
	wake := func(bearer, fn string) (int, string, http.Header) {
		t.Helper()
		r := httptest.NewRequest("POST", "/internal/functions/"+fn+"/wake", strings.NewReader("")).WithContext(e.ctx)
		if bearer != "" {
			r.Header.Set("Authorization", bearer)
		}
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, r)
		return w.Code, w.Body.String(), w.Header()
	}
	asleep := func(why string) {
		t.Helper()
		if fn := e.fn(id); fn.Instances != 0 || e.cluster.replicas("fn-p", id) != 0 {
			t.Fatalf("%s started something: record %d, cluster %d", why, fn.Instances, e.cluster.replicas("fn-p", id))
		}
	}

	// 401, and the same 401 whatever is asked for: a function that exists, one that does not, no id at all. With
	// the location unreadable too: had anything been looked up first, the answer would be another.
	e.svcErr = errors.New("kubeconfig: boom")
	var first string
	for _, bearer := range []string{"", "Bearer ", "Bearer nope", "Bearer " + fnWakeToken + "x", fnWakeToken, "Basic " + fnWakeToken} {
		for _, fn := range []string{id, "fn-0000000000000000", "not-an-id", other} {
			code, body, _ := wake(bearer, fn)
			if _, c, _ := envelope(t, body); code != 401 || c != 401 {
				t.Errorf("wake of %s with %q: %d %s, want 401", fn, bearer, code, body)
			}
			if first == "" {
				first = body
			} else if body != first {
				t.Errorf("wake of %s with %q answers %s, another 401 than %s", fn, bearer, body, first)
			}
		}
	}
	// A failed read is not "no such function": the proxy would remember a 404 and tell its callers so.
	if code, _, hdr := wake("Bearer "+fnWakeToken, id); code != 503 || hdr.Get("Retry-After") == "" {
		t.Errorf("wake while the location cannot be read: %d (Retry-After %q), want 503", code, hdr.Get("Retry-After"))
	}
	e.svcErr = nil
	asleep("a refused wake")

	// 404: no such function, no function id, and the function of another provider than the token's.
	for _, c := range [][2]string{{fnWakeToken, "fn-0000000000000000"}, {fnWakeToken, "sbx-0000000000000000"}, {fnWakeToken, other}, {"token-of-svc2", id}} {
		if code, body, _ := wake("Bearer "+c[0], c[1]); code != 404 {
			t.Errorf("wake of %s with the token %s: %d %s, want 404", c[1], c[0], code, body)
		}
	}
	asleep("a wake with another provider's token")
	if fn := e.fn(other); fn.Instances != 1 {
		t.Fatalf("the other provider's function was touched: %+v", fn)
	}

	// 503 while the job scheduler is off, or a release gate does not hold: nothing would stop or charge what starts.
	e.h.SetFunctionScheduler(false)
	if code, _, hdr := wake("Bearer "+fnWakeToken, id); code != 503 || hdr.Get("Retry-After") == "" {
		t.Errorf("wake with the job scheduler off: %d, want 503 with Retry-After", code)
	}
	if code, _ := e.call("owner-p", "POST", "/project/p/functions", `{"name":"two","image":"nginx:1"}`); code != 503 {
		t.Errorf("create with the job scheduler off: %d, want 503", code)
	}
	e.h.SetFunctionScheduler(true)
	e.gates = []sandbox.Gate{{Name: "fence", OK: false, Detail: "a machine is not fenced"}}
	e.provider(externalservice.StatusPublic, fnBlock())
	if code, body, _ := wake("Bearer "+fnWakeToken, id); code != 503 || strings.Contains(body, "fenced") {
		t.Errorf("wake while a release gate does not hold: %d %s, want 503 without the operator's reason", code, body)
	}
	e.gates = []sandbox.Gate{{Name: "fence", OK: true}}
	e.provider(externalservice.StatusPublic, fnBlock())
	asleep("a wake the gates refused")

	// 403: functions off where it lives (the block, or the provider), suspended, a project that cannot be billed.
	e.provider(externalservice.StatusPublic, map[string]any{"enabled": false})
	if code, _, _ := wake("Bearer "+fnWakeToken, id); code != 403 {
		t.Errorf("wake with the block off: %d, want 403", code)
	}
	e.provider(externalservice.StatusDisabled, fnBlock())
	if code, _, _ := wake("Bearer "+fnWakeToken, id); code != 403 {
		t.Errorf("wake on a disabled provider: %d, want 403", code)
	}
	e.provider(externalservice.StatusPublic, fnBlock())
	e.project(pgdoc.M{"status": "DISABLED"})
	if code, body, hdr := wake("Bearer "+fnWakeToken, id); code != 403 || hdr.Get("Retry-After") != "" || strings.Contains(body, "unpaid") {
		t.Errorf("wake in a project that cannot be billed: %d %s, want a plain 403", code, body)
	}
	e.project(pgdoc.M{"status": StatusEnabled})
	if code, _ := e.call("owner-p", "POST", "/project/p/functions/"+id+"/action", `{"action":"suspend"}`); code != 200 {
		t.Fatalf("suspend: %d", code)
	}
	if code, _, _ := wake("Bearer "+fnWakeToken, id); code != 403 {
		t.Errorf("wake of a suspended function: %d, want 403", code)
	}
	if code, _ := e.call("owner-p", "POST", "/project/p/functions/"+id+"/action", `{"action":"resume"}`); code != 200 {
		t.Fatalf("resume: %d", code)
	}
	asleep("a forbidden wake")

	// 409: nothing to run.
	broken := e.create(`{"name":"broken","image":"bad:1"}`)
	e.settle(broken, true)
	if code, _, _ := wake("Bearer "+fnWakeToken, broken); code != 409 || e.cluster.replicas("fn-p", broken) != 0 {
		t.Errorf("wake of a function whose only deployment failed: %d, want 409", code)
	}

	// The cluster: its quota is 429, anything else 503, both with Retry-After and none with the cluster's words.
	for want, fail := range map[int]error{
		429: &kamajik8s.APIError{Status: http.StatusForbidden, Message: `pods "x" is forbidden: exceeded quota: proxcloud-function in fn-other`},
		503: fmt.Errorf("%w: dial tcp 10.0.0.7:6443: i/o timeout", kamajik8s.ErrUnreachable),
	} {
		e.cluster.fail = map[string]error{"SetScale": fail}
		code, body, hdr := wake("Bearer "+fnWakeToken, id)
		if code != want || hdr.Get("Retry-After") == "" || strings.Contains(body, "fn-other") || strings.Contains(body, "10.0.0.7") {
			t.Errorf("wake while the cluster answers %v: %d %s, want %d", fail, code, body, want)
		}
		e.cluster.fail = nil
		// The record went up before the cluster was asked: an instance never runs unrecorded. Back to sleep.
		e.asleep(id)
	}

	// 200: one instance, one new interval. Again: the same instance, the same interval.
	before := e.fn(id)
	code, body, _ := wake("Bearer "+fnWakeToken, id)
	if data, _, _ := envelope(t, body); code != 200 || data["functionID"] != id || data["instances"] != float64(1) {
		t.Fatalf("wake: %d %s", code, body)
	}
	woken := e.fn(id)
	if woken.Instances != 1 || e.cluster.replicas("fn-p", id) != 1 || woken.StoppedAt != nil || woken.RunningSince == nil || !woken.RunningSince.After(*before.StoppedAt) ||
		woken.ReadyAt != nil || len(woken.Ended) != len(before.Ended) {
		t.Fatalf("after the wake: %+v", woken)
	}
	if row := e.row(id); row.Data["status"] != functionRowRunning || intAny(row.Data["instances"]) != 1 || row.Data["stoppedAt"] != nil || row.Data["readyAt"] != nil ||
		row.Data["runningSince"] != woken.RunningSince.Format(time.RFC3339) {
		t.Fatalf("the row after the wake: %v", row.Data)
	}
	if code, _, _ := wake("Bearer "+fnWakeToken, id); code != 200 {
		t.Fatalf("a second wake: %d", code)
	}
	if again := e.fn(id); again.Instances != 1 || !again.RunningSince.Equal(*woken.RunningSince) || len(again.Ended) != len(woken.Ended) {
		t.Fatalf("a second wake made a boundary: %+v", again)
	}
}

// The function token and the sandbox wake token are two: neither opens the other's route, and a provider that holds
// a function token and no sandbox wake token still refuses autoResume, whose only guard here is that absence.
func TestFunctionWakeTokensAreApart(t *testing.T) {
	e := newFnEnv(t)
	id := e.ready(`{"name":"api","image":"nginx:1"}`)
	sandboxWake := func(bearer string) int {
		r := httptest.NewRequest("POST", "/internal/sandboxes/sbx-0123456789abcdef/wake", strings.NewReader(`{}`)).WithContext(e.ctx)
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, r)
		return w.Code
	}
	secret := func(m map[string]any) {
		t.Helper()
		if _, err := e.db.C("externalService").SetByID(e.ctx, "svc", pgdoc.M{"secret": e.h.esSvc.EncryptSecret(m)}, nil); err != nil {
			t.Fatal(err)
		}
		e.h.ForgetFunctionTokens()
	}

	// The provider as a platform run leaves it: a function token, no sandbox wake token.
	if got := sandboxWake(fnWakeToken); got != 401 {
		t.Errorf("the function token on the sandbox wake route: %d, want 401", got)
	}
	es, err := e.h.esSvc.Get(e.ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	cfg := es.SandboxConfig()
	if cfg.FunctionWakeToken != fnWakeToken || cfg.ProxyWakeToken != "" {
		t.Fatalf("the provider's tokens: function %q, sandbox %q", cfg.FunctionWakeToken, cfg.ProxyWakeToken)
	}
	cfg.TemplateRegistry, cfg.BuildStorage = "reg/x", sandbox.BuildStorage{Bucket: "b", Endpoint: "http://s3"}
	paused := &Handler{sbTemplates: &pgdoc.Store{}, sbPaused: &pgdoc.Store{}}
	if err := paused.pauseRefusal(cfg, &sandbox.Spec{ProjectID: "p", AutoPause: true, AutoResume: true}); err == nil || !strings.Contains(err.Error(), "autoResume is not available") {
		t.Errorf("autoResume on a provider that holds only a function token: %v, want it refused for the missing sandbox wake token", err)
	}

	// Both tokens, different: each opens its own route only.
	secret(map[string]any{"functionWakeToken": fnWakeToken, "proxyWakeToken": "sandbox-wake-secret"})
	e.asleep(id)
	for _, path := range []string{id + "/wake", "activity"} {
		if got := e.proxy("sandbox-wake-secret", path, `{"ids":["`+id+`"]}`); got != 401 {
			t.Errorf("the sandbox wake token on %s: %d, want 401", path, got)
		}
	}
	if fn := e.fn(id); fn.Instances != 0 {
		t.Fatalf("the sandbox wake token woke a function: %+v", fn)
	}
	if got := sandboxWake(fnWakeToken); got != 401 {
		t.Errorf("the function token on the sandbox wake route: %d, want 401", got)
	}
	if got := sandboxWake("sandbox-wake-secret"); got == 401 {
		t.Errorf("the sandbox wake token on its own route: %d", got)
	}
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Errorf("the function token on its own route: %d", got)
	}

	// A provider without a function token has no bearer at all: not the empty one either.
	secret(map[string]any{"proxyWakeToken": "sandbox-wake-secret"})
	for _, bearer := range []string{"", fnWakeToken, "sandbox-wake-secret"} {
		if got := e.proxy(bearer, "activity", `{"ids":[]}`); got != 401 {
			t.Errorf("activity with %q on a provider without a function token: %d, want 401", bearer, got)
		}
	}

	// The tokens are kept in memory for a moment: a replaced token is still taken until they are read again.
	secret(map[string]any{"functionWakeToken": fnWakeToken})
	if got := e.proxy(fnWakeToken, "activity", `{"ids":[]}`); got != 204 {
		t.Fatal(got)
	}
	if _, err := e.db.C("externalService").SetByID(e.ctx, "svc", pgdoc.M{"secret": e.h.esSvc.EncryptSecret(map[string]any{"functionWakeToken": "rotated"})}, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.proxy(fnWakeToken, "activity", `{"ids":[]}`); got != 204 {
		t.Errorf("the token just replaced, before the tokens are read again: %d, want it still taken", got)
	}
	e.h.ForgetFunctionTokens()
	if old, rotated := e.proxy(fnWakeToken, "activity", `{"ids":[]}`), e.proxy("rotated", "activity", `{"ids":[]}`); old != 401 || rotated != 204 {
		t.Errorf("after the tokens were read again: the old one %d, the new one %d, want 401 and 204", old, rotated)
	}
}

// An activity report reaches the functions of the token's provider and no other: it stamps their activity, its load
// scales them, and its replica is on record for that provider alone. What it is given is bounded.
func TestFunctionActivity(t *testing.T) {
	e := newFnEnv(t)
	id := e.ready(`{"name":"api","image":"nginx:1","maxInstances":3,"concurrency":10}`)
	other := e.otherProvider("token-of-svc2")
	proxies := func() map[string]fnProxy {
		t.Helper()
		var docs []struct {
			ID string `json:"id"`
			fnProxy
		}
		if err := e.h.fnProxies().Find(e.ctx, pgdoc.M{"serviceId": pgdoc.M{"$exists": true}}, &docs); err != nil {
			t.Fatal(err)
		}
		out := map[string]fnProxy{}
		for _, d := range docs {
			out[d.ID] = d.fnProxy
		}
		return out
	}
	e.idle(id, time.Hour)
	e.idle(other, time.Hour)
	stale := *e.fn(id).LastActiveAt

	// Without the token nothing is taken.
	for _, bearer := range []string{"", "nope", "token-of-svc"} {
		if code, body := e.proxyRaw(bearer, "activity", `{"ids":["`+id+`"],"every":5,"replica":"r1","load":{"`+id+`":{"peak":30,"requests":9}}}`); code != 401 {
			t.Errorf("activity with %q: %d %s, want 401", bearer, code, body)
		}
	}
	if fn := e.fn(id); !fn.LastActiveAt.Equal(stale) || fn.Instances != 1 || len(proxies()) != 0 {
		t.Fatalf("a refused report left something: %+v, proxies %v", fn, proxies())
	}

	// A report names what it likes: its own function, another provider's, garbage. Only its own is reached.
	body := `{"ids":["` + id + `","` + other + `","garbage","fn-0000000000000000"],"every":5,"replica":"r1","load":{"` + id + `":{"peak":25,"requests":9},"` + other + `":{"peak":500,"requests":900}}}`
	if code, out := e.proxyRaw(fnWakeToken, "activity", body); code != 204 || out != "" {
		t.Fatalf("activity: %d %s", code, out)
	}
	if fn := e.fn(id); !fn.LastActiveAt.After(stale) || fn.Instances != 3 {
		t.Fatalf("the report's own function: lastActiveAt %v, instances %d, want it stamped and at three (25 in flight at 10 each)", fn.LastActiveAt, fn.Instances)
	}
	if fn := e.fn(other); time.Since(*fn.LastActiveAt) < 30*time.Minute || fn.Instances != 1 || fn.ScaledAt != nil {
		t.Fatalf("another provider's function was reached by the report: %+v", fn)
	}
	if got := proxies(); len(got) != 1 || got["svc/r1"].ServiceID != "svc" || got["svc/r1"].Every != 5 {
		t.Fatalf("the replicas on record: %+v, want r1 of svc alone", got)
	}
	if n, err := e.h.FunctionProxiesReporting(e.ctx, "svc2"); err != nil || n != 0 {
		t.Fatalf("the report counts as a proxy of the other provider: %d %v", n, err)
	}
	// The other provider's own report reaches its function, and not this one.
	e.idle(id, time.Hour)
	if code := e.proxy("token-of-svc2", "activity", `{"ids":["`+id+`","`+other+`"],"every":5,"replica":"r9"}`); code != 204 {
		t.Fatal(code)
	}
	if a, b := e.fn(id), e.fn(other); time.Since(*a.LastActiveAt) < 30*time.Minute || time.Since(*b.LastActiveAt) > time.Minute {
		t.Fatalf("the other provider's report: this function %v, its own %v", a.LastActiveAt, b.LastActiveAt)
	}

	// A partial report stamps and is no word on the rest: the replica's record stays as it was.
	at := proxies()["svc/r1"].At
	if code := e.proxy(fnWakeToken, "activity", `{"ids":["`+id+`"],"every":5,"replica":"r1","partial":true}`); code != 204 {
		t.Fatal(code)
	}
	if fn := e.fn(id); time.Since(*fn.LastActiveAt) > time.Minute || !proxies()["svc/r1"].At.Equal(at) {
		t.Fatalf("a partial report: lastActiveAt %v, the replica's record %v (was %v)", fn.LastActiveAt, proxies()["svc/r1"].At, at)
	}

	// Bounds. A body that does not decode is not echoed: it is where a caller could put anything.
	many := make([]string, maxActivityIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("fn-%016x", i)
	}
	tooMany, _ := json.Marshal(map[string]any{"ids": many, "replica": "r1"})
	for name, body := range map[string]string{
		"too many ids":       string(tooMany),
		"a replica too long": `{"ids":[],"replica":"` + strings.Repeat("r", 254) + `"}`,
		"a replica path":     `{"ids":[],"replica":"svc2/r1"}`,
		"the reaper's name":  `{"ids":[],"replica":"` + fnReaperID + `"}`,
		"no JSON":            `{"ids":["marker-in-a-bad-body"`,
		"a body past 64 KiB": `{"ids":[],"replica":"r1","pad":"` + strings.Repeat("x", 64<<10) + `"}`,
	} {
		code, out := e.proxyRaw(fnWakeToken, "activity", body)
		if _, c, _ := envelope(t, out); code != 400 || c != 400 || strings.Contains(out, "marker-in-a-bad-body") {
			t.Errorf("%s: %d %s, want 400", name, code, out)
		}
	}
	if got := proxies(); len(got) != 2 {
		t.Fatalf("a refused report wrote a replica's record: %v", got)
	}

	// The record is the proof the operator's gate asks for, for as long as it is kept and no longer: the deploy
	// sync forgets it at the same age the gate stops counting it.
	if n, err := e.h.FunctionProxiesReporting(e.ctx, "svc"); err != nil || n != 1 {
		t.Fatalf("proxies reporting: %d %v, want 1", n, err)
	}
	age := func(d time.Duration) {
		t.Helper()
		if _, err := e.h.fnProxies().SetFields(e.ctx, pgdoc.M{"_id": "svc/r1"}, pgdoc.M{"at": time.Now().UTC().Add(-d)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	age(85 * time.Second) // the window is 90 seconds: the number the gate's row prints
	e.sync()
	if n, _ := e.h.FunctionProxiesReporting(e.ctx, "svc"); n != 1 || len(proxies()) != 2 {
		t.Fatalf("a report inside the window: counted %d, records %v", n, proxies())
	}
	age(91 * time.Second)
	if n, _ := e.h.FunctionProxiesReporting(e.ctx, "svc"); n != 0 {
		t.Fatalf("a report past the window still counts: %d", n)
	}
	e.sync()
	if got := proxies(); len(got) != 1 || got["svc2/r9"].ServiceID != "svc2" {
		t.Fatalf("the deploy sync did not forget the replica that is gone: %v", got)
	}
}

// snapshot is everything the jobs may change of the functions: the records, their rows and the cluster's counts.
func (e *fnEnv) snapshot() string {
	e.t.Helper()
	var b strings.Builder
	for _, fn := range e.records() {
		record, _ := json.Marshal(fn)
		row, _ := json.Marshal(e.row(fn.ID).Data)
		fmt.Fprintf(&b, "%s\nrow %s\ncluster %d\n", record, row, e.cluster.replicas("fn-"+fn.ProjectID, fn.ID))
	}
	return b.String()
}

// The reaper ends an idle function at the moment its idle time ran out, not at the moment it got to it; the first
// run after it was away stops nothing for being idle; and a run on a state the run before it left changes nothing.
func TestFunctionReaperEndsAtTheIdleTime(t *testing.T) {
	e := newFnEnv(t)
	id := e.ready(`{"name":"api","image":"nginx:1","idleSeconds":300}`)
	backdate := func(id string, started, active time.Duration) (time.Time, time.Time) {
		t.Helper()
		now := time.Now().UTC().Truncate(time.Second)
		since, last := now.Add(-started), now.Add(-active)
		if _, err := e.h.fns.SetByID(e.ctx, id, pgdoc.M{"runningSince": since, "lastActiveAt": last}, nil); err != nil {
			t.Fatal(err)
		}
		return since, last
	}

	// A reaper that was away for two hours: the function's last request was two hours ago, its idle time five
	// minutes. The first run after the gap leaves it (the reports were away as long); the next ends it.
	since, last := backdate(id, 3*time.Hour, 2*time.Hour)
	if _, err := e.h.fnProxies().DeleteByID(e.ctx, fnReaperID); err != nil {
		t.Fatal(err)
	}
	e.h.ReapFunctions(e.ctx)
	if fn := e.fn(id); fn.Instances != 1 || fn.StoppedAt != nil {
		t.Fatalf("the first run of a reaper that has no run on record stopped an idle function: %+v", fn)
	}
	if err := e.h.fnProxies().Upsert(e.ctx, fnReaperID, pgdoc.M{"at": time.Now().UTC().Add(-functionReaperGap - time.Second)}); err != nil {
		t.Fatal(err)
	}
	e.h.ReapFunctions(e.ctx)
	if fn := e.fn(id); fn.Instances != 1 {
		t.Fatalf("the first run after a gap stopped an idle function: %+v", fn)
	}
	e.h.ReapFunctions(e.ctx) // the run after it: the reports had their period to arrive
	fn := e.fn(id)
	want := last.Add(300 * time.Second)
	if fn.Instances != 0 || fn.StoppedAt == nil || !fn.StoppedAt.Equal(want) {
		t.Fatalf("a late reaper: stopped at %v, want %v (the last activity plus the idle time), not now", fn.StoppedAt, want)
	}
	if n := len(fn.Ended); n != 1 || !fn.Ended[0].From.Equal(since) || !fn.Ended[0].To.Equal(want) {
		t.Fatalf("the ended interval: %+v, want %v to %v", fn.Ended, since, want)
	}
	if row := e.row(id); row.Data["stoppedAt"] != want.Format(time.RFC3339) || row.Data["runningSince"] != since.Format(time.RFC3339) || intAny(row.Data["instances"]) != 0 {
		t.Fatalf("the row of the late stop: %v", row.Data)
	}

	// An interval that began after the idle time ran out (a wake whose request never came back) ends where it
	// began: never before its own start.
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatal(got)
	}
	since, _ = backdate(id, time.Minute, 2*time.Hour)
	e.reap()
	if fn := e.fn(id); fn.Instances != 0 || !fn.StoppedAt.Equal(since) {
		t.Fatalf("an interval that began after its idle time ran out: stopped at %v, want its start %v", fn.StoppedAt, since)
	}

	// What is not the idle rule's ends now, and at once, also on the first run after a gap: off.
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatal(got)
	}
	e.provider(externalservice.StatusPublic, map[string]any{"enabled": false})
	if _, err := e.h.fnProxies().DeleteByID(e.ctx, fnReaperID); err != nil {
		t.Fatal(err)
	}
	e.h.ReapFunctions(e.ctx)
	if fn := e.fn(id); fn.Instances != 0 || time.Since(*fn.StoppedAt) > time.Minute {
		t.Fatalf("functions off, on the first run after a gap: %+v", fn)
	}
	e.provider(externalservice.StatusPublic, fnBlock())

	// Run twice, each job changes nothing the second time; nor does a report that is replayed.
	second := e.ready(`{"name":"two","image":"nginx:1","minInstances":1}`)
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatal(got)
	}
	e.idle(id, time.Hour)
	e.idle(second, time.Hour)
	e.reap()
	e.sync()
	settled := e.snapshot()
	for _, job := range []struct {
		name string
		run  func()
	}{{"the reaper", e.reap}, {"the deploy sync", e.sync}, {"the reaper after the sync", e.reap}, {"the deploy sync again", e.sync}} {
		job.run()
		if got := e.snapshot(); got != settled {
			t.Fatalf("%s changed a settled state:\nbefore %s\nafter  %s", job.name, settled, got)
		}
	}
	report := `{"ids":["` + second + `"],"every":5,"replica":"r1","load":{"` + second + `":{"peak":3,"requests":3}}}`
	if code := e.proxy(fnWakeToken, "activity", report); code != 204 {
		t.Fatal(code)
	}
	once := e.fn(second)
	if code := e.proxy(fnWakeToken, "activity", report); code != 204 {
		t.Fatal(code)
	}
	if twice := e.fn(second); twice.Instances != once.Instances || !twice.RunningSince.Equal(*once.RunningSince) || len(twice.Ended) != len(once.Ended) ||
		!reflect.DeepEqual(e.row(second).Data["runningSince"], once.RunningSince.Format(time.RFC3339)) {
		t.Fatalf("a replayed report changed the function: %+v, was %+v", twice, once)
	}
}

// A site that never had a function (a sandbox provider with the block off, no record, no row): the reaper and the
// deploy sync ask the cluster nothing and write nothing, run after run. The same provider with the block on is
// listed, which is what tells the two apart.
func TestFunctionJobsLeaveASiteWithoutFunctionsAlone(t *testing.T) {
	e := newFnEnv(t)
	off := fnBlock()
	off["enabled"] = false
	e.provider(externalservice.StatusPublic, off)
	untouched := func(why string) {
		t.Helper()
		e.cluster.mu.Lock()
		n, calls := len(e.cluster.calls), fmt.Sprint(e.cluster.calls)
		e.cluster.mu.Unlock()
		if n != 0 {
			t.Fatalf("%s: the cluster was called: %s", why, calls)
		}
		if n, err := e.h.fnProxies().Count(e.ctx, nil); err != nil || n != 0 {
			t.Fatalf("%s: %d records in function_proxies (%v), want none", why, n, err)
		}
		if state, err := e.h.cloud.MeterState(e.ctx, "svc"); err != nil || (state != nil && state.LastOKAt != nil) {
			t.Fatalf("%s: the provider's meter heartbeat was stamped: %+v (%v)", why, state, err)
		}
	}
	for range 2 {
		e.h.ReapFunctions(e.ctx)
		e.h.SyncFunctionDeployments(e.ctx)
	}
	untouched("block off")
	e.provider(externalservice.StatusDisabled, fnBlock())
	e.h.ReapFunctions(e.ctx)
	e.h.SyncFunctionDeployments(e.ctx)
	untouched("provider disabled")

	e.provider(externalservice.StatusPublic, fnBlock())
	e.h.SyncFunctionDeployments(e.ctx)
	e.cluster.mu.Lock()
	listed := len(e.cluster.calls)
	e.cluster.mu.Unlock()
	if listed == 0 {
		t.Fatal("with the block on the deploy sync did not look at the cluster")
	}
}

// Functions switched off on the provider, or the provider disabled: one run of the reaper later no function of it
// runs and no row says one does, whatever its traffic and its floor. They come back when functions do.
func TestFunctionOffStopsEverything(t *testing.T) {
	e := newFnEnv(t)
	busy := e.ready(`{"name":"busy","image":"nginx:1","maxInstances":3,"concurrency":10}`)
	floor := e.ready(`{"name":"floor","image":"nginx:1","minInstances":1}`)
	idle := e.ready(`{"name":"idle","image":"nginx:1"}`)
	e.asleep(idle)
	if code := e.proxy(fnWakeToken, "activity", `{"ids":["`+busy+`"],"every":5,"replica":"r1","load":{"`+busy+`":{"peak":30,"requests":30}}}`); code != 204 || e.fn(busy).Instances != 3 {
		t.Fatalf("the busy function: %d, %d instances", code, e.fn(busy).Instances)
	}
	off := func(why string) {
		t.Helper()
		e.reap()
		rows, err := e.h.cloud.FindByServiceAndType(e.ctx, "svc", cloud.TypeFunction)
		if err != nil || len(rows) != 3 {
			t.Fatalf("%s: rows %d %v", why, len(rows), err)
		}
		for _, row := range rows {
			if intAny(row.Data["instances"]) != 0 || row.Data["status"] != functionRowIdle || row.Data["stoppedAt"] == nil {
				t.Errorf("%s: the row of %s still says it runs: %v", why, row.ExternalID, row.Data)
			}
			if fn := e.fn(row.ExternalID); fn.Instances != 0 || e.cluster.replicas("fn-p", fn.ID) != 0 {
				t.Errorf("%s: %s runs %d on record, %d on the cluster", why, fn.Name, fn.Instances, e.cluster.replicas("fn-p", fn.ID))
			}
		}
		// Reports keep coming from a proxy that still has traffic: they start nothing.
		if code := e.proxy(fnWakeToken, "activity", `{"ids":["`+busy+`"],"every":5,"replica":"r1","load":{"`+busy+`":{"peak":30,"requests":30}}}`); code != 204 {
			t.Fatal(code)
		}
		e.reap()
		for _, id := range []string{busy, floor, idle} {
			if fn := e.fn(id); fn.Instances != 0 {
				t.Errorf("%s: %s was started again while functions are off", why, fn.Name)
			}
		}
	}
	e.provider(externalservice.StatusPublic, map[string]any{"enabled": false, "maxMinInstances": 1})
	off("the block switched off")
	e.provider(externalservice.StatusPublic, fnBlock())
	e.reap()
	if fn := e.fn(floor); fn.Instances != 1 || e.fn(busy).Instances != 0 {
		t.Fatalf("functions on again: the one with a floor runs %d (want 1), the busy one %d (want 0 until a request wakes it)", fn.Instances, e.fn(busy).Instances)
	}
	if got := e.proxy(fnWakeToken, busy+"/wake", ""); got != 200 {
		t.Fatal(got)
	}
	e.provider(externalservice.StatusDisabled, fnBlock())
	// Between the switch and the reaper's run the load adds nothing to what still runs.
	if code := e.proxy(fnWakeToken, "activity", `{"ids":["`+busy+`"],"every":5,"replica":"r1","load":{"`+busy+`":{"peak":30,"requests":30}}}`); code != 204 || e.fn(busy).Instances != 1 {
		t.Fatalf("a report while the provider is disabled: %d, the function at %d (want it left at 1)", code, e.fn(busy).Instances)
	}
	off("the provider disabled")
}

// intervalsHold checks what every writer of the instance count must leave: a running function has a start and no
// end, a stopped one an end, the ended intervals are apart and in order, and the row says what the record says.
func (e *fnEnv) intervalsHold(id, why string) functionDoc {
	e.t.Helper()
	fn, row := e.fn(id), e.row(id)
	switch {
	case fn.Instances > 0 && (fn.RunningSince == nil || fn.StoppedAt != nil):
		e.t.Fatalf("%s: runs %d with bounds %v to %v", why, fn.Instances, fn.RunningSince, fn.StoppedAt)
	case fn.Instances == 0 && fn.RunningSince != nil && (fn.StoppedAt == nil || fn.StoppedAt.Before(*fn.RunningSince)):
		e.t.Fatalf("%s: stopped with bounds %v to %v", why, fn.RunningSince, fn.StoppedAt)
	}
	for i, in := range fn.Ended {
		if in.To.Before(in.From) || (i > 0 && in.From.Before(fn.Ended[i-1].To)) {
			e.t.Fatalf("%s: ended intervals overlap or run backwards: %+v", why, fn.Ended)
		}
		if fn.Instances > 0 && !fn.RunningSince.After(in.From) {
			e.t.Fatalf("%s: the running interval %v is also among the ended ones: %+v", why, fn.RunningSince, fn.Ended)
		}
	}
	if n := len(fn.Ended); fn.Instances == 0 && fn.RunningSince != nil && (n == 0 || !fn.Ended[n-1].From.Equal(*fn.RunningSince) || !fn.Ended[n-1].To.Equal(*fn.StoppedAt)) {
		e.t.Fatalf("%s: the last interval %v to %v is not the last ended one: %+v", why, fn.RunningSince, fn.StoppedAt, fn.Ended)
	}
	ended, _ := row.Data["endedIntervals"].([]any)
	if functionRowStale(row.Data, e.h.functionRowData(e.ctx, &fn)) || len(ended) != len(fn.Ended) {
		e.t.Fatalf("%s: the row does not say what the record says:\nrow    %v\nrecord %+v", why, row.Data, fn)
	}
	return fn
}

// A wake and the reaper that meet on the same function leave one boundary or none, never two of a kind: many wakes
// of a sleeping function make one start, a wake that comes while the reaper stops an idle function finds it running
// (the stop yields, or ends one interval before the wake begins the next), and the row follows the record through
// all of it. One run of the deploy sync later the cluster runs what is on record.
func TestFunctionWakeAndReapRace(t *testing.T) {
	e := newFnEnv(t)
	id := e.ready(`{"name":"api","image":"nginx:1"}`)
	wake := func() {
		if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
			t.Errorf("wake: %d", got)
		}
	}
	// Many wakes of a function that sleeps: one start.
	e.asleep(id)
	before := e.intervalsHold(id, "asleep")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(wake)
	}
	wg.Wait()
	if fn := e.intervalsHold(id, "after eight wakes at once"); fn.Instances != 1 || len(fn.Ended) != len(before.Ended) || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatalf("eight wakes at once: %+v", fn)
	}
	// Many reapers on a function that idled out: one stop.
	e.idle(id, time.Hour)
	for range 4 {
		wg.Go(e.reap)
	}
	wg.Wait()
	if fn := e.intervalsHold(id, "after four reapers at once"); fn.Instances != 0 || len(fn.Ended) != len(before.Ended)+1 {
		t.Fatalf("four reapers at once: %+v", fn)
	}
	// A wake against the reaper, at the moment the idle time runs out, again and again.
	wake()
	for round := range 30 {
		prev := e.intervalsHold(id, "before the round")
		e.idle(id, time.Hour)
		wg.Go(e.reap)
		wg.Go(wake)
		wg.Wait()
		why := fmt.Sprintf("round %d", round)
		fn := e.intervalsHold(id, why)
		// The wake's request must find an instance: whatever the reaper did, the function is running afterwards.
		if fn.Instances != 1 {
			t.Fatalf("%s: the function is at %d after a wake", why, fn.Instances)
		}
		// One boundary of each kind at most: the reaper ended the interval and the wake began the next, or
		// the stop yielded and nothing changed.
		switch added := len(fn.Ended) - len(prev.Ended); {
		case added == 0 && !fn.RunningSince.Equal(*prev.RunningSince):
			t.Fatalf("%s: a new start without the end of the interval before it: %+v", why, fn)
		case added == 1 && !fn.RunningSince.After(*prev.RunningSince):
			t.Fatalf("%s: an end without the start that follows it: %+v", why, fn)
		case added > 1 || added < 0:
			t.Fatalf("%s: %d intervals ended in one round: %+v", why, added, fn.Ended)
		}
		e.sync()
		if got := e.cluster.replicas("fn-p", id); got != 1 {
			t.Fatalf("%s: the cluster runs %d after the deploy sync, the record says 1", why, got)
		}
	}
}

// A start that never serves is not its owner's cost and does not run on: the machines have no room, the namespace's
// quota refuses the pod, the container keeps exiting. The deploy sync notes it on one run and ends it on the next,
// as void, whatever its callers retry meanwhile; the next request starts again, and is ended again. A start that
// does serve is stamped ready, and one that served and then lost its instances is ended like any other interval.
func TestFunctionStartsThatNeverServe(t *testing.T) {
	e := newFnEnv(t)
	for name, knob := range map[string]*map[string]bool{"no room": &e.cluster.unplaced, "quota": &e.cluster.refused, "crash": &e.cluster.crashing} {
		id := e.ready(`{"name":"` + name[:2] + `","image":"nginx:1"}`)
		// The rollout that became ready stamped its interval: it served.
		if fn := e.fn(id); fn.ReadyAt == nil || e.row(id).Data["readyAt"] == nil {
			t.Fatalf("%s: a rollout that finished did not stamp readyAt: %+v, row %v", name, fn, e.row(id).Data)
		}
		e.asleep(id)
		served := len(e.fn(id).Ended)
		if served != 1 {
			t.Fatalf("%s: %d ended intervals after the first one", name, served)
		}
		retry := func() {
			t.Helper()
			if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
				t.Fatalf("%s: wake: %d", name, got)
			}
			if code := e.proxy(fnWakeToken, "activity", `{"ids":["`+id+`"],"every":5,"replica":"r1"}`); code != 204 {
				t.Fatal(code)
			}
		}
		void := func(why string) {
			t.Helper()
			fn, row := e.fn(id), e.row(id)
			if fn.Instances != 0 || fn.RunningSince != nil || fn.StoppedAt != nil || fn.ReadyAt != nil || fn.UnreadyAt != nil || len(fn.Ended) != served || e.cluster.replicas("fn-p", id) != 0 {
				t.Fatalf("%s, %s: not ended as void: %+v", name, why, fn)
			}
			ended, _ := row.Data["endedIntervals"].([]any)
			if intAny(row.Data["instances"]) != 0 || row.Data["status"] != functionRowIdle || row.Data["runningSince"] != nil || row.Data["stoppedAt"] != nil || row.Data["readyAt"] != nil || len(ended) != served {
				t.Fatalf("%s, %s: the row keeps something of a void interval: %v", name, why, row.Data)
			}
		}
		*knob = map[string]bool{id: true}
		for attempt := range 2 {
			why := fmt.Sprintf("attempt %d", attempt)
			retry()
			if fn, row := e.fn(id), e.row(id); fn.Instances != 1 || fn.ReadyAt != nil || row.Data["runningSince"] == nil || row.Data["readyAt"] != nil {
				t.Fatalf("%s, %s: after the wake: %+v, row %v", name, why, fn, row.Data)
			}
			// The first run only notes it: the instance may be on its way.
			e.sync()
			if fn := e.fn(id); fn.Instances != 1 || fn.UnreadyAt == nil || fn.ReadyAt != nil {
				t.Fatalf("%s, %s: the first run of the sync: %+v", name, why, fn)
			}
			// The callers keep retrying: to the idle rule it is as active as can be.
			retry()
			e.reap()
			if fn := e.fn(id); fn.Instances != 1 {
				t.Fatalf("%s, %s: the reaper stopped a function with traffic: %+v", name, why, fn)
			}
			// A run that comes too soon is not the next one.
			e.seenAgo(id, "unreadyAt", functionUnreadyAfter-5*time.Second)
			e.sync()
			if fn := e.fn(id); fn.Instances != 1 {
				t.Fatalf("%s, %s: ended before the next run of the sync: %+v", name, why, fn)
			}
			// The next run, a minute after the first: within two minutes of the start it is over, and void.
			e.seenAgo(id, "unreadyAt", time.Minute)
			e.sync()
			void(why)
		}
		// Room again: the next start serves, and the sync says so on the record and the row.
		*knob = nil
		retry()
		e.sync()
		fn, row := e.fn(id), e.row(id)
		if fn.Instances != 1 || fn.ReadyAt == nil || fn.UnreadyAt != nil || row.Data["readyAt"] != fn.ReadyAt.Format(time.RFC3339) {
			t.Fatalf("%s: a start that serves: %+v, row %v", name, fn, row.Data)
		}
		ready := *fn.ReadyAt
		e.sync()
		if again := e.fn(id); !again.ReadyAt.Equal(ready) {
			t.Fatalf("%s: readyAt was stamped twice: %v then %v", name, ready, again.ReadyAt)
		}
		// It served, then lost its instances: ended two runs later like any interval, billed to its end.
		*knob = map[string]bool{id: true}
		_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, 1) // the controller sees it
		e.sync()
		e.seenAgo(id, "unreadyAt", time.Minute)
		e.sync()
		if fn := e.fn(id); fn.Instances != 0 || fn.StoppedAt == nil || len(fn.Ended) != served+1 || e.row(id).Data["stoppedAt"] == nil || e.row(id).Data["readyAt"] == nil {
			t.Fatalf("%s: an interval that served and lost its instances: %+v, row %v", name, fn, e.row(id).Data)
		}
		// A ready instance that comes back after a first sighting takes the mark away again.
		*knob = nil
		retry()
		*knob = map[string]bool{id: true}
		_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, 1)
		e.sync()
		*knob = nil
		_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, 1)
		e.sync()
		if fn := e.fn(id); fn.Instances != 1 || fn.UnreadyAt != nil || fn.ReadyAt == nil {
			t.Fatalf("%s: an instance that became ready after a first sighting: %+v", name, fn)
		}
		if code, _ := e.call("owner-p", "DELETE", "/project/p/functions/"+id, ""); code != 202 {
			t.Fatal(code)
		}
	}

	// A first deployment that never becomes ready is the same: nothing of it is billed.
	id := e.create(`{"name":"broken","image":"bad:1"}`)
	if fn := e.fn(id); fn.Instances != 1 || fn.RunningSince == nil || fn.ReadyAt != nil {
		t.Fatalf("a first deployment in progress: %+v", fn)
	}
	e.settle(id, true)
	fn, row := e.fn(id), e.row(id)
	if fn.Instances != 0 || fn.RunningSince != nil || fn.StoppedAt != nil || len(fn.Ended) != 0 || row.Data["runningSince"] != nil || row.Data["stoppedAt"] != nil || row.Data["endedIntervals"] != nil {
		t.Fatalf("a failed first deployment left an interval: %+v, row %v", fn, row.Data)
	}
}

// A cluster that does not answer, or is gone, does not keep a function on record as running: a stop is recorded
// whatever the cluster says and repeated by the deploy sync, a platform run ends the intervals without any cluster
// call, and the provider's meter heartbeat moves only when the cluster was seen.
func TestFunctionClusterGone(t *testing.T) {
	e := newFnEnv(t)
	id := e.ready(`{"name":"api","image":"nginx:1"}`)
	down := fmt.Errorf("%w: dial tcp: i/o timeout", kamajik8s.ErrUnreachable)
	heartbeat := func() *time.Time {
		t.Helper()
		m, err := e.h.cloud.MeterState(e.ctx, "svc")
		if err != nil {
			t.Fatal(err)
		}
		if m == nil {
			return nil
		}
		return m.LastOKAt
	}
	if heartbeat() != nil {
		t.Fatalf("a heartbeat before the cluster was ever listed: %v", heartbeat())
	}

	// The reaper's stop while the cluster is down: on record, and in the row, at once.
	e.cluster.fail = map[string]error{"SetScale": down, "ListCRsAllNamespaces": down}
	e.idle(id, time.Hour)
	e.reap()
	if fn, row := e.fn(id), e.row(id); fn.Instances != 0 || fn.StoppedAt == nil || intAny(row.Data["instances"]) != 0 || row.Data["stoppedAt"] == nil || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatalf("a stop the cluster did not take: record %+v, row %v, cluster %d (want the record at zero, the cluster still at one)", fn, row.Data, e.cluster.replicas("fn-p", id))
	}
	e.intervalsHold(id, "after a stop the cluster did not take")
	// The sync cannot list the cluster: no heartbeat.
	e.sync()
	if heartbeat() != nil {
		t.Fatalf("a heartbeat without a list of the cluster: %v", heartbeat())
	}
	// The cluster is back: the sync repeats the stop, and the heartbeat moves.
	e.cluster.fail = nil
	e.sync()
	first := heartbeat()
	if e.cluster.replicas("fn-p", id) != 0 || first == nil || time.Since(*first) > time.Minute {
		t.Fatalf("after the cluster came back: cluster %d, heartbeat %v", e.cluster.replicas("fn-p", id), first)
	}
	// A list that fails later leaves the heartbeat where it was: billing sees how long nobody looked.
	old := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)
	if err := e.h.cloud.SetMeterFields(e.ctx, "svc", pgdoc.M{"lastOkAt": old}); err != nil {
		t.Fatal(err)
	}
	e.cluster.fail = map[string]error{"ListCRsAllNamespaces": down}
	e.sync()
	if got := heartbeat(); got == nil || !got.Equal(old) {
		t.Fatalf("the heartbeat moved without a list: %v, want %v", got, old)
	}
	e.cluster.fail = nil

	// A suspend while the cluster is down: the caller is told, and the function is on record as stopped.
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatal(got)
	}
	e.cluster.fail = map[string]error{"SetScale": down}
	if code, _ := e.call("owner-p", "POST", "/project/p/functions/"+id+"/action", `{"action":"suspend"}`); code != 503 {
		t.Errorf("suspend while the cluster is down: %d, want 503", code)
	}
	if fn := e.fn(id); !fn.Suspended || fn.Instances != 0 || e.row(id).Data["status"] != functionRowSuspended {
		t.Fatalf("after a suspend the cluster did not take: %+v", fn)
	}
	e.cluster.fail = nil
	if code, _ := e.call("owner-p", "POST", "/project/p/functions/"+id+"/action", `{"action":"resume"}`); code != 200 {
		t.Fatal(code)
	}

	// A platform run that is about to delete the cluster ends every running interval in the records, and asks
	// the cluster nothing.
	if exist, err := e.h.FunctionsExist(e.ctx, "svc"); err != nil || !exist {
		t.Fatalf("FunctionsExist of a provider with a function: %v %v", exist, err)
	}
	if exist, err := e.h.FunctionsExist(e.ctx, "another"); err != nil || exist {
		t.Fatalf("FunctionsExist of a provider without one: %v %v", exist, err)
	}
	second := e.ready(`{"name":"two","image":"nginx:1"}`)
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatal(got)
	}
	e.provider(externalservice.StatusDisabled, fnBlock())
	e.cluster.mu.Lock()
	calls := fmt.Sprint(e.cluster.calls)
	e.cluster.mu.Unlock()
	e.cluster.fail = map[string]error{"SetScale": down, "ListCRsAllNamespaces": down}
	if err := e.h.EndFunctions(e.ctx, "svc"); err != nil {
		t.Fatal(err)
	}
	e.cluster.mu.Lock()
	after := fmt.Sprint(e.cluster.calls)
	e.cluster.mu.Unlock()
	if after != calls {
		t.Fatalf("EndFunctions called the cluster: %s, was %s", after, calls)
	}
	for _, f := range []string{id, second} {
		if fn := e.intervalsHold(f, "after EndFunctions"); fn.Instances != 0 || fn.StoppedAt == nil || intAny(e.row(f).Data["instances"]) != 0 {
			t.Fatalf("EndFunctions left %s running: %+v", fn.Name, fn)
		}
	}
	if n := len(e.records()); n != 2 {
		t.Fatalf("EndFunctions dropped a record: %d left", n)
	}
	if err := e.h.EndFunctions(e.ctx, "svc"); err != nil {
		t.Fatalf("EndFunctions must be repeatable: %v", err)
	}
}

// The deploy sync gives a cluster its functions back and takes away what nothing on record needs: a Deployment that
// is gone is applied again from its record at the recorded count, with its secrets, also when the whole namespace is
// gone (a sandbox cluster that was rebuilt); a function namespace whose project is gone, or that holds no function
// any more, is deleted. Not where functions are off, and never a namespace that is not a function namespace of this
// provider.
func TestFunctionSyncRestoresAndSweeps(t *testing.T) {
	e := newFnEnv(t)
	e.set(map[string]any{"maxFunctions": 5})
	sleeping := e.ready(`{"name":"sleeping","image":"nginx:1","secrets":{"TOKEN":"kept-value"}}`)
	running := e.ready(`{"name":"running","image":"nginx:2"}`)
	e.asleep(sleeping)
	if got := e.proxy(fnWakeToken, "activity", `{"ids":["`+running+`"],"every":5,"replica":"r1"}`); got != 204 {
		t.Fatal(got)
	}
	restored := func(why string) {
		t.Helper()
		e.sync()
		for id, want := range map[string]int{sleeping: 0, running: 1} {
			if got := e.cluster.replicas("fn-p", id); got != want {
				t.Fatalf("%s: %s runs %d on the cluster, the record says %d", why, e.fn(id).Name, got, want)
			}
			if fn := e.intervalsHold(id, why); fn.Instances != want || depImage(e.cluster.get("fn-p", id)) != fn.Image {
				t.Fatalf("%s: the record changed, or another image was applied: %+v", why, fn)
			}
		}
		if _, values := e.cluster.secret("fn-p", sleeping); values["TOKEN"] != "kept-value" {
			t.Fatalf("%s: the function's secret was not applied again: %v", why, values)
		}
		// The run after it finds the running one ready again and takes the first sighting away.
		e.sync()
		if fn := e.fn(running); fn.Instances != 1 || fn.UnreadyAt != nil || fn.ReadyAt == nil {
			t.Fatalf("%s: the running function after its instance came back: %+v", why, fn)
		}
	}
	for _, id := range []string{sleeping, running} {
		_ = e.cluster.DeleteCR(e.ctx, "", "", "", "fn-p", id)
	}
	restored("Deployments deleted behind the portal")
	_ = e.cluster.DeleteNamespace(e.ctx, "fn-p")
	restored("the namespace gone, as on a cluster that was rebuilt")
	if e.cluster.quota("fn-p") == nil {
		t.Fatal("the namespace came back without its quota")
	}

	// Not where functions are off, and not a function that has nothing that served or is being deployed.
	never := e.create(`{"name":"never","image":"bad:1"}`)
	e.settle(never, true)
	deploying := e.ready(`{"name":"deploying","image":"nginx:1"}`)
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+deploying, `{"image":"nginx:3"}`); code != 200 {
		t.Fatal(code)
	}
	for _, id := range []string{never, deploying} {
		_ = e.cluster.DeleteCR(e.ctx, "", "", "", "fn-p", id)
	}
	e.sync()
	for _, id := range []string{never, deploying} {
		if got := e.cluster.replicas("fn-p", id); got != -1 {
			t.Errorf("the Deployment of %s was applied by the sync (%d instances)", e.fn(id).Name, got)
		}
	}
	e.settle(deploying, true)
	e.set(map[string]any{"enabled": false})
	_ = e.cluster.DeleteCR(e.ctx, "", "", "", "fn-p", sleeping)
	e.sync()
	if got := e.cluster.replicas("fn-p", sleeping); got != -1 {
		t.Errorf("a Deployment was applied where functions are off (%d instances)", got)
	}
	e.set(map[string]any{"enabled": true})

	// The sweep.
	labels := func(service, project string) map[string]string {
		return map[string]string{sandbox.LabelManagedBy: sandbox.ManagedByValue, sandbox.LabelService: service, sandbox.LabelProject: project, sandbox.LabelTenant: "true"}
	}
	now := time.Now()
	e.cluster.mu.Lock()
	e.cluster.namespaces["fn-gone"] = labels("svc", "gone")                // its project is gone
	e.cluster.namespaces["fn-foreign"] = labels("svc", "foreign")          // its project has no function here, and it is old
	e.cluster.namespaces["fn-young"] = labels("svc", "young")              // the same, made a moment ago
	e.cluster.namespaces["fn-undated"] = labels("svc", "undated")          // the same, and the cluster tells no age
	e.cluster.namespaces["fn-elsewhere"] = labels("another", "elsewhere")  // another provider's
	e.cluster.namespaces["sb-gone"] = labels("svc", "gone")                // a sandbox namespace
	e.cluster.namespaces["fn-mislabelled"] = labels("svc", "someone-else") // not the namespace of the project it names
	e.cluster.nsCreated = map[string]time.Time{"fn-foreign": now.Add(-functionNamespaceGrace - time.Minute), "fn-young": now.Add(-time.Minute),
		"fn-gone": now.Add(-time.Minute), "fn-p": now.Add(-time.Hour)}
	e.cluster.mu.Unlock()
	for _, id := range []string{"young", "undated"} {
		p := &Project{ID: id, Status: StatusEnabled, OrganizationID: "org"}
		if err := e.db.C("project").Upsert(e.ctx, id, p); err != nil {
			t.Fatal(err)
		}
	}
	e.sync()
	e.cluster.mu.Lock()
	var left []string
	for name := range e.cluster.namespaces {
		left = append(left, name)
	}
	e.cluster.mu.Unlock()
	want := []string{"fn-elsewhere", "fn-mislabelled", "fn-p", "fn-undated", "fn-young", "sb-gone"}
	if slices.Sort(left); !slices.Equal(left, want) {
		t.Fatalf("namespaces after the sweep: %v, want %v", left, want)
	}
}
