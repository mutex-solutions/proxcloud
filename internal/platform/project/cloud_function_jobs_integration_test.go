//go:build integration

package project

// The tests of a function's life that pass through the jobs and the proxy's routes: the reaper, the wake, the
// activity report, the rollout poller and the deploy sync behind it, the floor, and the reconcile of record, row and
// cluster. On real PostgreSQL with the cluster faked (fnEnv).

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/function"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestFunctionLifecycle(t *testing.T) {
	e := newFnEnv(t)
	base := "/project/p/functions"

	// Access and validation before anything exists.
	for _, tc := range []struct {
		sub, method, path, body string
		want                    int
	}{
		{"outsider", "GET", base, "", 404},
		{"reader", "POST", base, `{"name":"api","image":"nginx:1"}`, 403},
		{"owner-p", "POST", base, `{"name":"Not A Label","image":"nginx:1"}`, 400},
		{"owner-p", "POST", base, `{"name":"api","image":"nginx:1","cpuMillis":300}`, 400},
		{"owner-p", "POST", base, `{"name":"api","image":"nginx:1","minInstances":2}`, 400},
		{"owner-p", "GET", base + "/fn-0000000000000000", "", 404},
		{"owner-p", "GET", base + "/not-an-id", "", 404},
	} {
		if code, _ := e.call(tc.sub, tc.method, tc.path, tc.body); code != tc.want {
			t.Errorf("%s %s %s (%s): %d, want %d", tc.sub, tc.method, tc.path, tc.body, code, tc.want)
		}
	}

	// Create: a namespace, a Deployment with one instance, a DEPLOYING record, a billed row.
	code, data := e.call("owner-p", "POST", base, `{"name":"api","image":"nginx:1","env":{"TOKEN":"s3cret"}}`)
	if code != 200 || data["status"] != fnDeploying || data["port"] != float64(8080) || data["cpuMillis"] != float64(250) {
		t.Fatalf("create: %d %v", code, data)
	}
	id, host := data["id"].(string), data["host"].(string)
	if !function.ValidID(id) || !strings.HasPrefix(host, "api-") || !strings.HasSuffix(host, ".fn.example") || data["url"] != "https://"+host {
		t.Fatalf("identity: %v", data)
	}
	if e.cluster.namespaces["fn-p"][function.LabelTenant] != "true" || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatalf("cluster after create: namespaces=%v replicas=%d", e.cluster.namespaces, e.cluster.replicas("fn-p", id))
	}
	fn := e.fn(id)
	if fn.Instances != 1 || fn.DeployingID == "" || fn.CurrentDeploymentID != "" || fn.RunningSince == nil || fn.LastActiveAt == nil {
		t.Fatalf("record after create: %+v", fn)
	}
	if row := e.row(id); row == nil || row.Type != cloud.TypeFunction || row.ProjectID != "p" || row.Data["status"] != functionRowRunning ||
		row.Data["name"] != "api" || row.Data["runningSince"] == nil || intAny(row.Data["instances"]) != 1 || intAny(row.Data["cpuMillis"]) != 250 {
		t.Fatalf("cloud row after create: %+v", row)
	}
	if deps := e.deployments(id); len(deps) != 1 || deps[0].State != depDeploying || deps[0].Seq != 1 || deps[0].Image != "nginx:1" || deps[0].Env["TOKEN"] != "s3cret" {
		t.Fatalf("deployments after create: %+v", deps)
	}
	if code, _ := e.call("owner-p", "POST", base, `{"name":"api","image":"nginx:1"}`); code != 409 {
		t.Errorf("duplicate name: %d, want 409", code)
	}
	if code, _ := e.call("owner-foreign", "GET", base+"/"+id, ""); code != 404 {
		t.Errorf("another project's owner: %d, want 404", code)
	}

	// The rollout completes: ACTIVE, the function is READY.
	if !e.settle(id, false) {
		t.Fatal("a finished rollout must settle")
	}
	if fn = e.fn(id); fn.DeployingID != "" || fn.CurrentDeploymentID == "" || fn.CurrentSeq != 1 || fn.DeployedAt == nil {
		t.Fatalf("record after rollout: %+v", fn)
	}
	first := fn.CurrentDeploymentID
	code, data = e.call("owner-p", "GET", base+"/"+id, "")
	if code != 200 || data["status"] != fnReady || data["instances"] != float64(1) || data["env"].(map[string]any)["TOKEN"] != "s3cret" ||
		data["currentDeployment"].(map[string]any)["seq"] != float64(1) {
		t.Fatalf("get: %d %v", code, data)
	}

	// A bad image: the redeploy fails, the previous snapshot is re-applied, the function keeps serving.
	code, data = e.call("owner-p", "PATCH", base+"/"+id, `{"image":"bad:1","cpuMillis":1000,"memoryMi":1024}`)
	if code != 200 || data["status"] != fnDeploying || !strings.HasPrefix(fmt.Sprint(data["reason"]), "image pull failed") || strings.Contains(fmt.Sprint(data), "hunter2") {
		t.Fatalf("patch to a bad image: %d %v", code, data)
	}
	if code, _ := e.call("owner-p", "PATCH", base+"/"+id, `{"image":"nginx:9"}`); code != 409 {
		t.Errorf("a second rollout while one is in progress: %d, want 409", code)
	}
	// The rollout's instance runs at the new size from now on, whether or not it ever becomes
	// ready: billing reads the larger of the serving and the deploying size.
	if row := e.row(id); intAny(row.Data["cpuMillis"]) != 1000 || intAny(row.Data["memoryMi"]) != 1024 {
		t.Fatalf("cloud row during a rollout to a larger size: %+v", row.Data)
	}
	// Suspending now would settle the rollout at zero instances and record the unproven
	// deployment as the working one.
	if code, _ := e.call("owner-p", "POST", base+"/"+id+"/action", `{"action":"suspend"}`); code != 409 || e.fn(id).Suspended {
		t.Errorf("suspend during a rollout: %d, want 409", code)
	}
	if got := depImage(e.cluster.get("fn-p", id)); got != "bad:1" {
		t.Fatalf("cluster image during the rollout = %s", got)
	}
	// A progress-deadline failure seen seconds into a rollout is the previous rollout's: not believed.
	if e.settle(id, false) {
		t.Fatal("a failure seen right after the rollout started must not settle it")
	}
	if !e.settle(id, true) {
		t.Fatal("the final poll must settle")
	}
	fn = e.fn(id)
	if fn.DeployingID != "" || fn.CurrentDeploymentID != first || fn.Image != "nginx:1" || !strings.HasPrefix(fn.LastError, "image pull failed") {
		t.Fatalf("record after a failed rollout: %+v", fn)
	}
	if got := depImage(e.cluster.get("fn-p", id)); got != "nginx:1" {
		t.Fatalf("cluster image after a failed rollout = %s, want the previous snapshot", got)
	}
	if deps := e.deployments(id); len(deps) != 2 || deps[0].State != depFailed || deps[0].Message == "" || deps[1].State != depActive {
		t.Fatalf("deployments after a failed rollout: %+v", deps)
	}
	if row := e.row(id); intAny(row.Data["cpuMillis"]) != 250 || intAny(row.Data["memoryMi"]) != 256 {
		t.Fatalf("cloud row after a failed rollout must be back at the serving size: %+v", row.Data)
	}
	if code, data = e.call("owner-p", "GET", base+"/"+id, ""); data["status"] != fnReady || data["lastError"] == nil {
		t.Fatalf("get after a failed rollout: %d %v", code, data)
	}

	// A good redeploy with env replaced; the old deployment is superseded.
	if code, _ = e.call("owner-p", "PATCH", base+"/"+id, `{"image":"nginx:2","env":{"A":"1"},"memoryMi":512}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("patch: %d", code)
	}
	fn = e.fn(id)
	if fn.Image != "nginx:2" || fn.MemoryMi != 512 || len(fn.Env) != 1 || fn.Env["A"] != "1" || fn.CurrentSeq != 3 || fn.LastError != "" {
		t.Fatalf("record after redeploy: %+v", fn)
	}
	if row := e.row(id); intAny(row.Data["memoryMi"]) != 512 || row.Data["image"] != "nginx:2" {
		t.Fatalf("cloud row must follow the deployed size: %+v", row.Data)
	}
	deps := e.deployments(id)
	if len(deps) != 3 || deps[0].State != depActive || deps[1].State != depFailed || deps[2].State != depSuperseded || deps[2].ID != first {
		t.Fatalf("deployments after redeploy: %+v", deps)
	}

	// idleSeconds / minInstances change without a deployment; a rename is refused.
	if code, _ = e.call("owner-p", "PATCH", base+"/"+id, `{"idleSeconds":120,"minInstances":1}`); code != 200 {
		t.Fatalf("patch idleSeconds: %d", code)
	}
	if fn = e.fn(id); fn.IdleSeconds != 120 || fn.MinInstances != 1 || fn.DeployingID != "" || len(e.deployments(id)) != 3 || fn.Env["A"] != "1" {
		t.Fatalf("record after a scale-policy patch: %+v", fn)
	}
	if code, _ = e.call("owner-p", "PATCH", base+"/"+id, `{"name":"other"}`); code != 400 {
		t.Errorf("rename: %d, want 400", code)
	}
	if code, _ = e.call("owner-p", "PATCH", base+"/"+id, `{"minInstances":0}`); code != 200 {
		t.Fatalf("patch minInstances back: %d", code)
	}

	// Rollback: only to a deployment that once served; it rolls out as a new deployment.
	for body, want := range map[string]int{
		`{"deploymentId":"` + deps[1].ID + `"}`: 400, // FAILED
		`{"deploymentId":"` + deps[0].ID + `"}`: 400, // the serving one
		`{"deploymentId":"nope"}`:               404,
	} {
		if code, _ := e.call("owner-p", "POST", base+"/"+id+"/rollback", body); code != want {
			t.Errorf("rollback %s: %d, want %d", body, code, want)
		}
	}
	if code, _ = e.call("owner-p", "POST", base+"/"+id+"/rollback", `{"deploymentId":"`+first+`"}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("rollback: %d", code)
	}
	fn = e.fn(id)
	// The image is the digest that deployment ran, not the tag it named.
	if !strings.HasPrefix(fn.Image, "nginx@sha256:") || fn.MemoryMi != 256 || fn.Env["TOKEN"] != "s3cret" || fn.CurrentSeq != 4 {
		t.Fatalf("record after rollback: %+v", fn)
	}
	code, w := 0, consoleSessionRequest(e.router, e.ctx, "owner-p", "GET", base+"/"+id+"/deployments", "")
	if code = w.Code; code != 200 || !strings.Contains(w.Body.String(), `"rollbackOf":1`) || strings.Count(w.Body.String(), `"seq"`) != 4 {
		t.Fatalf("deployments: %d %s", code, w.Body.String())
	}

	// Activity keeps an idle function from the reaper; without it the function scales to zero.
	e.idle(id, time.Hour)
	if got := e.proxy("nope", "activity", `{"ids":["`+id+`"]}`); got != 401 {
		t.Errorf("activity with a wrong token: %d, want 401", got)
	}
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 0 {
		t.Fatalf("idle function after the reaper: %d instances, want 0", got)
	}
	fn = e.fn(id)
	if fn.Instances != 0 || fn.StoppedAt == nil {
		t.Fatalf("record after reap: %+v", fn)
	}
	if row := e.row(id); row.Data["status"] != functionRowIdle || intAny(row.Data["instances"]) != 0 || row.Data["stoppedAt"] == nil || row.Data["runningSince"] == nil {
		t.Fatalf("cloud row after reap: %+v", row.Data)
	}
	// The interval that ended is kept, on the record and on the row, for a charge tick that does
	// not see the function stopped. While it is stopped billing gets it once, as the row's own.
	if n := len(fn.Ended); n == 0 || !fn.Ended[n-1].From.Equal(*fn.RunningSince) || !fn.Ended[n-1].To.Equal(*fn.StoppedAt) || fn.Ended[n-1].Instances != 1 ||
		fn.Ended[n-1].CPUMillis != fn.CPUMillis || fn.Ended[n-1].MemoryMi != fn.MemoryMi {
		t.Fatalf("ended intervals after reap: %+v", fn.Ended)
	}
	if _, data = e.call("owner-p", "GET", base+"/"+id, ""); data["status"] != fnIdle || data["instances"] != float64(0) {
		t.Fatalf("get after reap: %v", data)
	}

	// Wake: the location's token only; sets the scale, starts a new billed interval.
	for _, tc := range []struct {
		bearer, id string
		want       int
	}{{"", id, 401}, {"nope", id, 401}, {fnWakeToken, "fn-0000000000000000", 404}, {fnWakeToken, "sbx-0000000000000000", 404}, {fnWakeToken, id, 200}, {fnWakeToken, id, 200}} {
		if got := e.proxy(tc.bearer, tc.id+"/wake", ""); got != tc.want {
			t.Errorf("wake %q %s: %d, want %d", tc.bearer, tc.id, got, tc.want)
		}
	}
	// A failed read is not "no such function": the proxy would remember a 404 and tell callers so.
	e.svcErr = fmt.Errorf("kubeconfig: boom")
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 503 {
		t.Errorf("wake while the location cannot be read: %d, want 503", got)
	}
	e.svcErr = nil
	woken := e.fn(id)
	if e.cluster.replicas("fn-p", id) != 1 || woken.Instances != 1 || woken.StoppedAt != nil || !woken.RunningSince.After(*fn.RunningSince) {
		t.Fatalf("after wake: replicas=%d record=%+v", e.cluster.replicas("fn-p", id), woken)
	}
	if row := e.row(id); row.Data["status"] != functionRowRunning || row.Data["stoppedAt"] != nil {
		t.Fatalf("cloud row after wake: %+v", row.Data)
	}
	// The interval the wake ended the idle time of stays on the record and the row, ahead of the new one: a charge
	// tick that comes only now still finds it.
	if ended, _ := e.row(id).Data["endedIntervals"].([]any); len(woken.Ended) != len(fn.Ended) || len(ended) != len(fn.Ended) {
		t.Fatalf("ended intervals after wake: record %+v, row %v", woken.Ended, ended)
	}
	e.idle(id, time.Hour)
	if got := e.proxy(fnWakeToken, "activity", `{"ids":["`+id+`","garbage","fn-0000000000000000"]}`); got != 204 {
		t.Fatalf("activity: %d, want 204", got)
	}
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 1 {
		t.Fatalf("a function with reported activity was reaped (%d instances)", got)
	}

	// A start from a copy read before a stop (a wake that read the function while the reaper stopped it) still
	// publishes the interval that stop ended: the row carries it for the charge.
	stale := e.fn(id)
	e.idle(id, time.Hour)
	e.reap()
	if err := e.h.recordInstances(e.ctx, &stale, 1, fnStop{}); err != nil {
		t.Fatal(err)
	}
	fn = e.fn(id)
	if ended, _ := e.row(id).Data["endedIntervals"].([]any); fn.Instances != 1 || len(fn.Ended) != len(woken.Ended)+1 || len(ended) != len(fn.Ended) ||
		!fn.Ended[len(fn.Ended)-1].From.Equal(*woken.RunningSince) {
		t.Fatalf("after a start from a stale copy: record %+v, row %v", fn.Ended, ended)
	}

	// A wake repairs a function whose real scale drifted from the record.
	_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, 0)
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatalf("wake with the record already at one: %d, replicas %d", got, e.cluster.replicas("fn-p", id))
	}

	// Suspend: stopped, marked for the proxy, not wakeable. Resume: wakeable again.
	if code, data = e.call("owner-p", "POST", base+"/"+id+"/action", `{"action":"suspend"}`); code != 200 || data["status"] != fnSuspended {
		t.Fatalf("suspend: %d %v", code, data)
	}
	ann := e.cluster.get("fn-p", id)["metadata"].(map[string]any)["annotations"].(map[string]any)
	if e.cluster.replicas("fn-p", id) != 0 || ann[function.AnnotationSuspended] != "true" || e.row(id).Data["status"] != functionRowSuspended {
		t.Fatalf("after suspend: replicas=%d annotations=%v row=%v", e.cluster.replicas("fn-p", id), ann, e.row(id).Data)
	}
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 403 {
		t.Errorf("wake of a suspended function: %d, want 403", got)
	}
	if code, _ = e.call("owner-p", "POST", base+"/"+id+"/action", `{"action":"restart"}`); code != 400 {
		t.Errorf("unknown action: %d, want 400", code)
	}
	if code, data = e.call("owner-p", "POST", base+"/"+id+"/action", `{"action":"resume"}`); code != 200 || data["status"] != fnIdle {
		t.Fatalf("resume: %d %v", code, data)
	}
	ann = e.cluster.get("fn-p", id)["metadata"].(map[string]any)["annotations"].(map[string]any)
	if _, marked := ann[function.AnnotationSuspended]; marked {
		t.Fatalf("resume must clear the proxy's mark: %v", ann)
	}

	// A project that cannot be billed: no wake, no new function, no redeploy.
	e.project(pgdoc.M{"status": "DISABLED"})
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 403 || e.cluster.replicas("fn-p", id) != 0 {
		t.Errorf("wake in a project that cannot be billed: %d (replicas %d), want 403", got, e.cluster.replicas("fn-p", id))
	}
	if code, _ = e.call("owner-p", "POST", base, `{"name":"two","image":"nginx:1"}`); code != 409 {
		t.Errorf("create in a project that cannot be billed: %d, want 409", code)
	}
	if code, _ = e.call("owner-p", "PATCH", base+"/"+id, `{"image":"nginx:3"}`); code != 409 {
		t.Errorf("redeploy in a project that cannot be billed: %d, want 409", code)
	}
	e.project(pgdoc.M{"status": StatusEnabled})
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatalf("wake after resume: %d", got)
	}

	// Logs and list.
	if code, data = e.call("owner-p", "GET", base+"/"+id+"/logs?tail=50", ""); code != 200 || data["logs"] != "log of pod-1 tail=50\n" {
		t.Fatalf("logs: %d %v", code, data)
	}
	w = consoleSessionRequest(e.router, e.ctx, "owner-p", "GET", base, "")
	if w.Code != 200 || strings.Count(w.Body.String(), `"id":"fn-`) != 1 || !strings.Contains(w.Body.String(), `"status":"READY"`) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}

	// Delete while running: everything goes at once, and the row is archived DELETED with its end stamped: the
	// archived row is what billing rates to the end of the last interval.
	if code, _ = e.call("owner-p", "DELETE", base+"/"+id, ""); code != 202 {
		t.Fatalf("delete: %d", code)
	}
	if found, _ := e.h.fns.Get(e.ctx, id, &fn); found || e.cluster.replicas("fn-p", id) != -1 || len(e.deployments(id)) != 0 {
		t.Fatalf("after delete: record found=%v replicas=%d deployments=%d", found, e.cluster.replicas("fn-p", id), len(e.deployments(id)))
	}
	if gone := e.archived(id); e.row(id) != nil || len(gone) != 1 || gone[0].Data["status"] != functionRowDeleted || gone[0].Data["stoppedAt"] == nil || intAny(gone[0].Data["instances"]) != 0 {
		t.Fatalf("cloud row after delete: live %+v, archived %+v", e.row(id), gone)
	}
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 404 {
		t.Errorf("wake of a deleted function: %d, want 404", got)
	}
	// Its host stays taken for ever: a later function can never be handed the same URL.
	if _, err := e.h.fnHosts.InsertOne(e.ctx, pgdoc.M{"_id": host}); !pgdoc.IsDup(err) {
		t.Errorf("the host of a deleted function can be issued again (insert: %v)", err)
	}
	e.sync()
	if len(e.archived(id)) != 1 || e.row(id) != nil {
		t.Fatal("the deploy sync changed the archived row of a deleted function")
	}
}

// A first deployment that never becomes ready leaves a FAILED function at zero instances, which
// cannot be woken (it would be billed for a crash loop) until a deployment succeeds.
func TestFunctionFirstDeploymentFails(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"broken","image":"bad:1"}`)
	if !e.settle(id, true) {
		t.Fatal("the final poll must settle")
	}
	fn := e.fn(id)
	if fn.CurrentDeploymentID != "" || fn.DeployingID != "" || fn.Instances != 0 || fn.LastError == "" || e.cluster.replicas("fn-p", id) != 0 {
		t.Fatalf("after a failed first deployment: record=%+v replicas=%d", fn, e.cluster.replicas("fn-p", id))
	}
	if _, data := e.call("owner-p", "GET", "/project/p/functions/"+id, ""); data["status"] != fnFailed || data["currentDeployment"] != nil {
		t.Fatalf("get: %v", data)
	}
	// It never had a ready instance: its start is no interval, on the record or the row. Nothing of it is billed.
	if row := e.row(id); fn.RunningSince != nil || fn.StoppedAt != nil || len(fn.Ended) != 0 || row.Data["runningSince"] != nil || row.Data["stoppedAt"] != nil || row.Data["endedIntervals"] != nil {
		t.Fatalf("a failed first deployment left a running interval: %+v, row %v", fn, row.Data)
	}
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 409 || e.cluster.replicas("fn-p", id) != 0 {
		t.Fatalf("wake of a function with nothing to run: %d", got)
	}
	// A floor of one does not start it either.
	e.set(map[string]any{"minInstances": 1})
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 0 {
		t.Fatalf("the reaper raised a function with no working deployment to %d", got)
	}
	e.set(map[string]any{"minInstances": 0})
	// The failure may have had nothing to do with the config (the image was pushed late, the
	// registry was down): the same config deploys again, also with an empty update.
	for _, body := range []string{`{}`, `{"image":"bad:1"}`} {
		code, data := e.call("owner-p", "PATCH", "/project/p/functions/"+id, body)
		if code != 200 || data["status"] != fnDeploying || e.cluster.replicas("fn-p", id) != 1 {
			t.Fatalf("redeploy of a failed first deployment with %s: %d %v", body, code, data)
		}
		if !e.settle(id, true) {
			t.Fatal("the final poll must settle")
		}
	}
	if deps := e.deployments(id); len(deps) != 3 || e.fn(id).Instances != 0 || e.cluster.replicas("fn-p", id) != 0 {
		t.Fatalf("after two retries: %d deployments, record %+v", len(deps), e.fn(id))
	}
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:1"}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("fixing deploy: %d", code)
	}
	if _, data := e.call("owner-p", "GET", "/project/p/functions/"+id, ""); data["status"] != fnReady || data["lastError"] != nil {
		t.Fatalf("get after the fix: %v", data)
	}
}

// The floor (reaper), the per-project function limit, deployment pruning, teardown, feature off.
func TestFunctionFloorLimitsAndTeardown(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"one","image":"nginx:1","minInstances":1}`)
	e.settle(id, false)

	// The function's own floor keeps it up when idle; lowering what the platform allows takes
	// effect at the next reap, with no redeploy.
	e.idle(id, time.Hour)
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 1 {
		t.Fatalf("idle function with minInstances 1: %d instances, want 1", got)
	}
	e.set(map[string]any{"maxMinInstances": 0})
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 0 {
		t.Fatalf("after the platform stopped allowing a floor: %d instances, want 0", got)
	}
	// The platform floor raises it again without any traffic.
	e.set(map[string]any{"minInstances": 1})
	e.reap()
	if fn := e.fn(id); e.cluster.replicas("fn-p", id) != 1 || fn.Instances != 1 || fn.StoppedAt != nil {
		t.Fatalf("platform floor of one: replicas=%d record=%+v", e.cluster.replicas("fn-p", id), fn)
	}
	// A resume restores the floor itself, without waiting for the reaper: where the console says
	// one instance of every function is kept running, a resumed function is not left at zero.
	action := func(a string) {
		t.Helper()
		if code, data := e.call("owner-p", "POST", "/project/p/functions/"+id+"/action", `{"action":"`+a+`"}`); code != 200 {
			t.Fatalf("%s: %d %v", a, code, data)
		}
	}
	action("suspend")
	if fn := e.fn(id); e.cluster.replicas("fn-p", id) != 0 || fn.Instances != 0 {
		t.Fatalf("suspended: replicas=%d record=%+v", e.cluster.replicas("fn-p", id), fn)
	}
	action("resume")
	if fn := e.fn(id); e.cluster.replicas("fn-p", id) != 1 || fn.Instances != 1 || fn.StoppedAt != nil || e.row(id).Data["status"] != functionRowRunning {
		t.Fatalf("resumed with a floor of one: replicas=%d record=%+v row=%v", e.cluster.replicas("fn-p", id), fn, e.row(id).Data)
	}
	// Until that instance is ready the function is IDLE with none, and says that one is starting
	// or what holds it up: the answer to the resume, and a read after it. Not the list, which has
	// no reason, and not once the instance is ready.
	action("suspend")
	e.cluster.starting = map[string]bool{id: true}
	fnPath := "/project/p/functions/" + id
	code, data := e.call("owner-p", "POST", fnPath+"/action", `{"action":"resume"}`)
	if code != 200 || data["status"] != fnIdle || data["instances"] != float64(0) || data["reason"] != function.ReasonStarting || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatalf("resume with a floor of one, the instance not ready yet: %d %v", code, data)
	}
	e.cluster.starting, e.cluster.unplaced = nil, map[string]bool{id: true}
	if _, data = e.call("owner-p", "GET", fnPath, ""); data["status"] != fnIdle || data["reason"] != function.ReasonUnplaced {
		t.Fatalf("an instance that gets no node: %v", data)
	}
	if w := consoleSessionRequest(e.router, e.ctx, "owner-p", "GET", "/project/p/functions", ""); strings.Contains(w.Body.String(), `"reason"`) {
		t.Fatalf("the list carries a reason: %s", w.Body.String())
	}
	e.cluster.unplaced = nil
	action("suspend")
	if code, data = e.call("owner-p", "POST", fnPath+"/action", `{"action":"resume"}`); code != 200 || data["status"] != fnReady || data["reason"] != nil {
		t.Fatalf("resume with a floor of one, the instance ready: %d %v", code, data)
	}
	// Not in a project that cannot be billed: a resume is refused there, and nothing starts.
	action("suspend")
	e.project(pgdoc.M{"status": "DISABLED"})
	if code, _ := e.call("owner-p", "POST", fnPath+"/action", `{"action":"resume"}`); code != 403 && code != 409 {
		t.Fatalf("resume in a project that cannot be billed: %d, want it refused", code)
	}
	if fn := e.fn(id); e.cluster.replicas("fn-p", id) != 0 || fn.Instances != 0 || !fn.Suspended {
		t.Fatalf("after a refused resume: replicas=%d record=%+v", e.cluster.replicas("fn-p", id), fn)
	}
	e.project(pgdoc.M{"status": StatusEnabled})
	action("resume")
	if got := e.cluster.replicas("fn-p", id); got != 1 {
		t.Fatalf("resumed once the project can be billed: %d instances, want the floor of 1", got)
	}
	// A project that cannot be billed has no floor and no running function, however busy: a warm
	// function never needs a wake, so the wake gate alone would let it run for ever.
	e.project(pgdoc.M{"status": "DISABLED"})
	e.idle(id, 0)
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 0 {
		t.Fatalf("busy function with a floor in a project that cannot be billed: %d instances, want 0", got)
	}
	// The reaper restores the floor once the project can be billed again.
	e.project(pgdoc.M{"status": StatusEnabled})
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 1 {
		t.Fatalf("the reaper restores the floor once the project can be billed: %d instances, want 1", got)
	}
	e.set(map[string]any{"minInstances": 0})
	// A size the platform no longer allows: the scale settings can still be changed, a rollout
	// cannot be started.
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"cpuMillis":1000}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("deploy a larger size: %d", code)
	}
	e.set(map[string]any{"maxCpuMillis": 500})
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"idleSeconds":120}`); code != 200 || e.fn(id).IdleSeconds != 120 || e.fn(id).DeployingID != "" {
		t.Fatalf("scale-only update of a function above the lowered size limit: %d", code)
	}
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"env":{"A":"1"}}`); code != 400 {
		t.Fatalf("rollout of a size above the limit: %d, want 400", code)
	}
	e.set(map[string]any{"maxCpuMillis": 0})
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"cpuMillis":250}`); code != 200 || !e.settle(id, false) {
		t.Fatalf("deploy the small size again: %d", code)
	}
	// A function with a rollout in progress is left alone, however idle.
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:2"}`); code != 200 {
		t.Fatal(code)
	}
	e.idle(id, time.Hour)
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 1 {
		t.Fatalf("the reaper touched a function that is deploying (%d instances)", got)
	}
	e.settle(id, false)

	// Keep the 10 newest deployment records.
	for i := 5; i <= 13; i++ {
		if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, fmt.Sprintf(`{"image":"nginx:%d"}`, i)); code != 200 || !e.settle(id, false) {
			t.Fatalf("deploy %d: %d", i, code)
		}
	}
	if deps := e.deployments(id); len(deps) != keepDeployments || deps[0].Seq != 13 || deps[9].Seq != 4 || deps[0].State != depActive {
		t.Fatalf("kept %d deployments, newest seq %d, oldest seq %d", len(deps), deps[0].Seq, deps[len(deps)-1].Seq)
	}

	// maxFunctions is 3 per project.
	e.create(`{"name":"two","image":"nginx:1"}`)
	e.create(`{"name":"three","image":"nginx:1"}`)
	if code, _ := e.call("owner-p", "POST", "/project/p/functions", `{"name":"four","image":"nginx:1"}`); code != 429 {
		t.Errorf("a fourth function: %d, want 429", code)
	}

	// Functions turned off on the location (the same for an invalid block or a disabled
	// provider): what runs is stopped, and so no longer billed, however busy; nothing can be
	// woken; and the project can still be torn down.
	e.set(map[string]any{"enabled": false})
	e.idle(id, 0)
	e.reap()
	if fn := e.fn(id); e.cluster.replicas("fn-p", id) != 0 || fn.Instances != 0 || e.row(id).Data["status"] != functionRowIdle {
		t.Fatalf("with functions off: replicas=%d record=%+v row=%v", e.cluster.replicas("fn-p", id), fn, e.row(id).Data)
	}
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 403 || e.cluster.replicas("fn-p", id) != 0 {
		t.Fatalf("wake with functions off: %d, want 403", got)
	}

	// Teardown: the namespace (and so every instance), the records and the rows.
	if err := e.h.DeleteProjectFunctions(e.ctx, "p"); err != nil {
		t.Fatal(err)
	}
	n, _ := e.h.fns.Count(e.ctx, pgdoc.M{"projectId": "p"})
	d, _ := e.h.fnDeps.Count(e.ctx, pgdoc.M{"projectId": "p"})
	rows, _ := e.h.cloud.FindByProjectAndType(e.ctx, "p", cloud.TypeFunction)
	if _, ns := e.cluster.namespaces["fn-p"]; ns || len(e.cluster.deps) != 0 || n != 0 || d != 0 || len(rows) != 0 {
		t.Fatalf("after teardown: namespace=%v deployments=%d records=%d deployment records=%d rows=%d", ns, len(e.cluster.deps), n, d, len(rows))
	}
	if err := e.h.DeleteProjectFunctions(e.ctx, "p"); err != nil {
		t.Fatalf("teardown must be repeatable: %v", err)
	}

	// Functions off: what is left can be read, nothing can be made.
	for path, want := range map[string]int{"GET /project/p/functions": 200, "POST /project/p/functions": 403, "GET /project/p/functions/" + id: 404} {
		method, path, _ := strings.Cut(path, " ")
		if code, _ := e.call("owner-p", method, path, `{"name":"x","image":"nginx:1"}`); code != want {
			t.Errorf("%s %s with functions off: %d, want %d", method, path, code, want)
		}
	}
}

// The real poller settles a rollout by itself; the scheduler's backstop settles one whose poller
// died, including one that died between committing the outcome and updating the function.
func TestFunctionPollerAndBackstop(t *testing.T) {
	e := newFnEnv(t)
	functionPollEvery = 10 * time.Millisecond
	id := e.create(`{"name":"polled","image":"nginx:1"}`)
	deadline := time.Now().Add(5 * time.Second)
	for e.fn(id).CurrentDeploymentID == "" {
		if time.Now().After(deadline) {
			t.Fatalf("the poller did not settle the rollout: %+v", e.fn(id))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if deps := e.deployments(id); len(deps) != 1 || deps[0].State != depActive || deps[0].FinishedAt == nil {
		t.Fatalf("deployments after the poller: %+v", deps)
	}

	functionPollEvery = time.Hour // from here on the poller is dead
	stale := func() {
		if _, err := e.h.fns.SetByID(e.ctx, id, pgdoc.M{"updatedAt": time.Now().UTC().Add(-functionStuckAfter - time.Second)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:2"}`); code != 200 {
		t.Fatal(code)
	}
	e.sync()
	if e.fn(id).DeployingID == "" {
		t.Fatal("the backstop must leave a fresh rollout to its poller")
	}
	stale()
	e.sync()
	if fn := e.fn(id); fn.DeployingID != "" || fn.Image != "nginx:2" || fn.CurrentSeq != 2 {
		t.Fatalf("after the backstop: %+v", fn)
	}

	// Crash after the outcome was committed: the record says ACTIVE, the function still holds the claim.
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:3"}`); code != 200 {
		t.Fatal(code)
	}
	dep := e.deployments(id)[0]
	now := time.Now().UTC()
	if _, err := e.h.fnDeps.SetByID(e.ctx, dep.ID, pgdoc.M{"state": depActive, "finishedAt": now}, nil); err != nil {
		t.Fatal(err)
	}
	stale()
	e.sync()
	if fn := e.fn(id); fn.DeployingID != "" || fn.Image != "nginx:3" || fn.CurrentDeploymentID != dep.ID {
		t.Fatalf("after the backstop finished a committed rollout: %+v", fn)
	}
	if deps := e.deployments(id); deps[0].State != depActive || deps[1].State != depSuperseded || deps[2].State != depSuperseded {
		t.Fatalf("deployment states: %s %s %s", deps[0].State, deps[1].State, deps[2].State)
	}

	// A bad rollout without a poller: failed by the backstop, previous snapshot restored.
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"bad:4"}`); code != 200 {
		t.Fatal(code)
	}
	stale()
	e.sync()
	if fn := e.fn(id); fn.DeployingID != "" || fn.Image != "nginx:3" || fn.LastError == "" || depImage(e.cluster.get("fn-p", id)) != "nginx:3" {
		t.Fatalf("after the backstop failed a rollout: %+v, cluster image %s", fn, depImage(e.cluster.get("fn-p", id)))
	}

	// A claim whose deployment record was never written (a crash between the two) is released.
	if _, err := e.h.fns.SetByID(e.ctx, id, pgdoc.M{"deployingId": "never-written"}, nil); err != nil {
		t.Fatal(err)
	}
	stale()
	e.sync()
	if fn := e.fn(id); fn.DeployingID != "" {
		t.Fatalf("a claim without a record must be released: %+v", fn)
	}

	// A row whose function is gone but was never ended (a write that raced the delete) is ended and archived: the
	// archived row is what billing rates to the end of the last interval.
	_, _ = e.h.fns.DeleteByID(e.ctx, id)
	e.sync()
	if gone := e.archived(id); e.row(id) != nil || len(gone) != 1 || gone[0].Data["status"] != functionRowDeleted || gone[0].Data["stoppedAt"] == nil {
		t.Fatalf("orphan row after the sweep: live %+v, archived %+v", e.row(id), gone)
	}
	// ... and so is its Deployment, which nothing would route, bill or reap.
	if got := e.cluster.replicas("fn-p", id); got != -1 {
		t.Fatalf("a Deployment without a function record must be deleted (replicas %d)", got)
	}
}

// Rows, records and the cluster are written one after the other, by several API replicas, from
// copies read earlier. Whatever a race or a crash between two steps leaves behind is repaired.
func TestFunctionReconcile(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"one","image":"nginx:1"}`)
	e.settle(id, false)

	// A wake that read the record before the reaper stopped the function, and writes after it:
	// the write itself must see that the function is starting again (a new billed interval).
	stale := e.fn(id) // instances 1
	first := *stale.RunningSince
	e.idle(id, time.Hour)
	e.reap()
	if fn := e.fn(id); fn.Instances != 0 || fn.StoppedAt == nil {
		t.Fatalf("after reap: %+v", fn)
	}
	if err := e.h.scaleFunction(e.ctx, e.fs(), &stale, 1); err != nil {
		t.Fatal(err)
	}
	fn := e.fn(id)
	if fn.Instances != 1 || fn.StoppedAt != nil || !fn.RunningSince.After(first) || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatalf("scale up from a stale copy: %+v", fn)
	}
	if row := e.row(id); row.Data["status"] != functionRowRunning || intAny(row.Data["instances"]) != 1 || row.Data["stoppedAt"] != nil {
		t.Fatalf("cloud row after a scale up from a stale copy: %+v", row.Data)
	}
	// ... and the reaper losing the same race the other way: it must not stamp a stop twice.
	stale = e.fn(id)
	e.idle(id, time.Hour)
	e.reap()
	stopped := e.fn(id).StoppedAt
	if err := e.h.scaleFunction(e.ctx, e.fs(), &stale, 0); err != nil || !e.fn(id).StoppedAt.Equal(*stopped) {
		t.Fatalf("a second stop moved the end of the interval: %v, %v != %v", err, e.fn(id).StoppedAt, stopped)
	}

	// Cluster above the record (the wake's scale landed after the reaper's record): an instance
	// that is neither billed nor ever reaped. The cluster follows the record.
	_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, 1)
	e.reap()
	if got := e.cluster.replicas("fn-p", id); got != 1 {
		t.Fatalf("precondition: the reaper cannot see this drift (replicas %d)", got)
	}
	e.sync()
	if got := e.cluster.replicas("fn-p", id); got != 0 {
		t.Fatalf("an instance running at a recorded zero: %d replicas after the sync, want 0", got)
	}
	// Cluster below the record: billed without running. Same rule.
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatal(got)
	}
	_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, 0)
	e.sync()
	if got := e.cluster.replicas("fn-p", id); got != 1 {
		t.Fatalf("a recorded instance that does not run: %d replicas after the sync, want 1", got)
	}

	// A row that lost a write (a crash between the record and the row): rewritten from the record.
	row := e.row(id)
	now := time.Now().UTC()
	row.Data["instances"], row.Data["status"], row.UpdatedAt = 0, functionRowIdle, &now
	if _, err := e.h.cloud.Update(e.ctx, row); err != nil {
		t.Fatal(err)
	}
	e.sync()
	if row := e.row(id); row.Data["status"] != functionRowRunning || intAny(row.Data["instances"]) != 1 {
		t.Fatalf("a row out of line with its record after the sync: %+v", row.Data)
	}

	// A rollout in progress is the deploy flow's: not touched, though the cluster is ahead.
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:2"}`); code != 200 {
		t.Fatal(code)
	}
	_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, 0)
	e.sync()
	if got := e.cluster.replicas("fn-p", id); got != 0 {
		t.Fatalf("the sync scaled a function that is deploying (replicas %d)", got)
	}
	_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, 1)
	e.settle(id, false)

	// A delete racing a deploy: the deploy's apply lands after the delete removed the Deployment
	// and the record. The apply finds the record gone and removes the Deployment again.
	fn = e.fn(id)
	if code, _ := e.call("owner-p", "DELETE", "/project/p/functions/"+id, ""); code != 202 {
		t.Fatal(code)
	}
	if err := e.h.applyFunction(e.ctx, e.fs(), &fn, fn.Snapshot); err != function.ErrNotFound || e.cluster.replicas("fn-p", id) != -1 {
		t.Fatalf("apply after delete: %v, replicas %d; want not found and no Deployment", err, e.cluster.replicas("fn-p", id))
	}
	// And the backstop, for one that got through anyway (a failed delete, a crash).
	if err := e.fs().Apply(e.ctx, fn.identity(), fn.Snapshot, nil, ""); err != nil || e.cluster.replicas("fn-p", id) != 1 {
		t.Fatal(err)
	}
	e.sync()
	if got := e.cluster.replicas("fn-p", id); got != -1 {
		t.Fatalf("a Deployment without a function record after the sync: replicas %d, want none", got)
	}
}

// lockedBuffer collects log output from any goroutine.
