//go:build integration

package project

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// report sends one complete activity report of an sandbox-proxy replica to an API replica (its
// router): the functions with traffic and the peak of requests in flight of each.
func (e *fnEnv) report(router http.Handler, replica string, peaks map[string]int) int {
	e.t.Helper()
	ids, load := []string{}, map[string]any{}
	for id, p := range peaks {
		ids, load[id] = append(ids, id), map[string]int{"peak": p, "requests": 10 * p}
	}
	body, _ := json.Marshal(map[string]any{"ids": ids, "every": 5, "replica": replica, "load": load})
	r := httptest.NewRequest("POST", "/internal/functions/activity", strings.NewReader(string(body))).WithContext(e.ctx)
	r.Header.Set("Authorization", "Bearer "+fnWakeToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w.Code
}

// scaledAgo makes the function's last scale change, and the last time its load asked for every
// instance, d ago: the stabilisation window is passed without waiting for it. Its load was still
// looked at a moment ago (loadAt), as with reports that keep coming.
func (e *fnEnv) scaledAgo(id string, d time.Duration) {
	e.t.Helper()
	at := time.Now().UTC().Add(-d)
	if _, err := e.h.fns.SetByID(e.ctx, id, pgdoc.M{"scaledAt": at, "busyAt": at}, nil); err != nil {
		e.t.Fatal(err)
	}
}

// at says the function runs n instances, on the record, the cluster and the billed row (which,
// stopped, keeps billing the count it last ran with).
func (e *fnEnv) at(id string, n int, why string) {
	e.t.Helper()
	fn, row := e.fn(id), e.row(id)
	if fn.Instances != n || e.cluster.replicas("fn-p", id) != n || intAny(row.Data["instances"]) != n || (n > 0 && intAny(row.Data["billedInstances"]) != n) {
		e.t.Fatalf("%s: record %d, cluster %d, row %v/%v; want %d", why, fn.Instances, e.cluster.replicas("fn-p", id), row.Data["instances"], row.Data["billedInstances"], n)
	}
}

// Load reports from two sandbox-proxy replicas add up; a function scales up to what they ask at once,
// to its cap and no further, and down one instance per stabilisation window, never below one and
// never while the load asks for every instance it has. A function created without maxInstances
// stays at one whatever its load.
func TestFunctionScaleOnLoad(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"api","image":"nginx:1","maxInstances":4,"concurrency":10}`)
	old := e.create(`{"name":"old","image":"nginx:1"}`)
	e.settle(id, false)
	e.settle(old, false)
	if _, data := e.call("owner-p", "GET", "/project/p/functions/"+old, ""); data["maxInstances"] != float64(1) || data["concurrency"] != float64(50) {
		t.Fatalf("the defaults: %v", data)
	}
	for body, want := range map[string]int{
		`{"maxInstances":6}`:                 400, // the location's cap is 5
		`{"concurrency":0,"maxInstances":0}`: 200, // 0 is the default
		`{"concurrency":1001}`:               400,
	} {
		if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+old, body); code != want {
			t.Errorf("patch %s: %d, want %d", body, code, want)
		}
	}

	// 15 in flight through one replica at a concurrency of 10: two instances, at once.
	if code := e.report(e.router, "r1", map[string]int{id: 15, old: 500}); code != 204 {
		t.Fatalf("report: %d", code)
	}
	e.at(id, 2, "15 requests in flight")
	e.at(old, 1, "a function with the default cap") // whatever its load
	if hard := e.cluster.quota("fn-p"); hard["pods"] != "18" {
		t.Errorf("the namespace quota has room for a scale up: %v, want 3 functions × (5 + 1) pods", hard)
	}
	if _, data := e.call("owner-p", "GET", "/project/p/functions/"+id, ""); data["instances"] != float64(2) || data["status"] != fnReady {
		t.Fatalf("get: %v", data)
	}
	// The second replica's 18 add to the first's 15: 33 → four (ceil 3.3), the cap.
	e.report(e.router, "r2", map[string]int{id: 18})
	e.at(id, 4, "33 requests in flight over two replicas")
	e.report(e.router, "r2", map[string]int{id: 900})
	e.at(id, 4, "held to maxInstances")

	// Less load: not before the window has passed, then one instance per window.
	e.report(e.router, "r1", map[string]int{id: 3})
	e.report(e.router, "r2", map[string]int{id: 2})
	e.at(id, 4, "less load inside the window")
	for _, want := range []int{3, 2, 1, 1} {
		e.scaledAgo(id, 2*functionScaleDownWindow)
		e.report(e.router, "r1", map[string]int{id: 3})
		e.at(id, want, "a window later")
		e.report(e.router, "r1", map[string]int{id: 3})
		e.at(id, want, "the same window") // the step just made starts a new one
	}
	if fn := e.fn(id); fn.LastActiveAt == nil || time.Since(*fn.LastActiveAt) > time.Minute {
		t.Fatalf("the reports keep the function from the idle rule: %+v", fn.LastActiveAt)
	}

	// Load that asks for every instance the function has keeps it there, past the window.
	e.report(e.router, "r1", map[string]int{id: 28}) // and r2's 2: three
	e.at(id, 3, "30 requests in flight")
	e.scaledAgo(id, 2*functionScaleDownWindow)
	e.report(e.router, "r1", map[string]int{id: 28})
	e.at(id, 3, "the load still asks for three")
	e.report(e.router, "r1", map[string]int{id: 5})
	e.at(id, 3, "the load asked for three a moment ago")

	// A replica not heard from for two periods adds nothing; nor does a proxy from before
	// several instances, which sends no load (and is answered as before).
	if _, err := e.h.fnProxies().SetFields(e.ctx, pgdoc.M{"_id": "svc/r1"}, pgdoc.M{"at": time.Now().UTC().Add(-11 * time.Second)}, nil); err != nil {
		t.Fatal(err)
	}
	e.scaledAgo(id, 2*functionScaleDownWindow)
	if got := e.proxy(fnWakeToken, "activity", `{"ids":["`+id+`"]}`); got != 204 {
		t.Fatalf("a report without load: %d", got)
	}
	e.at(id, 2, "a step down: r1's load is old, r2's is 2 and the old proxy's is none")
}

// Idle is idle at any count: the reaper takes a function at three instances straight to its floor
// (zero, or one), down the cluster first. The row bills the end of the interval at three. Suspend
// stops all of them, resume starts the floor, and the scale reconcile repairs drift both ways.
func TestFunctionScaleIdleSuspendReconcile(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"api","image":"nginx:1","maxInstances":3,"concurrency":10}`)
	e.settle(id, false)
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 3, "30 in flight")

	e.idle(id, time.Hour)
	e.reap()
	e.at(id, 0, "idle")
	if fn, row := e.fn(id), e.row(id); fn.StoppedAt == nil || fn.LastInstances != 3 || intAny(row.Data["billedInstances"]) != 3 || row.Data["status"] != functionRowIdle {
		t.Fatalf("stopped from three: lastInstances %d, row %v", fn.LastInstances, row.Data)
	}
	// A wake starts one, as ever; billing follows the count that runs.
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatalf("wake: %d", got)
	}
	e.at(id, 1, "woken")
	// The floor: idle from three goes to one.
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"minInstances":1}`); code != 200 {
		t.Fatalf("minInstances: %d", code)
	}
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 3, "30 in flight again")
	e.idle(id, time.Hour)
	e.reap()
	e.at(id, 1, "idle with a floor of one")
	if fn := e.fn(id); fn.LastActiveAt == nil || time.Since(*fn.LastActiveAt) < 30*time.Minute {
		t.Fatalf("a step down is no activity: lastActiveAt %v", fn.LastActiveAt)
	}
	// A lowered cap: the reaper brings a busy function down to it.
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 3, "back at three")
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"maxInstances":2}`); code != 200 {
		t.Fatalf("maxInstances: %d", code)
	}
	e.reap()
	e.at(id, 2, "a cap lowered to two")

	// Suspend → zero; resume → the floor.
	if code, _ := e.call("owner-p", "POST", "/project/p/functions/"+id+"/action", `{"action":"suspend"}`); code != 200 {
		t.Fatalf("suspend: %d", code)
	}
	e.at(id, 0, "suspended")
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 0, "load does not start a suspended function")
	if code, _ := e.call("owner-p", "POST", "/project/p/functions/"+id+"/action", `{"action":"resume"}`); code != 200 {
		t.Fatalf("resume: %d", code)
	}
	e.at(id, 1, "resumed to the floor")

	// The record is what runs: the reconcile brings the cluster back to it, either way.
	e.report(e.router, "r1", map[string]int{id: 20})
	e.at(id, 2, "20 in flight")
	for _, drift := range []int{1, 5} {
		_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", id, drift)
		e.sync()
		e.at(id, 2, "after the reconcile")
	}
}

// A function that runs several instances rolls them all out, one surge pod at a time and none
// unavailable, and keeps its count; the load does not change it while the rollout is in progress.
func TestFunctionScaleRollout(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"api","image":"nginx:1","maxInstances":5,"concurrency":10}`)
	e.settle(id, false)
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 3, "30 in flight")
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:2"}`); code != 200 {
		t.Fatalf("deploy: %d", code)
	}
	dep := e.cluster.get("fn-p", id)
	strategy := dep["spec"].(map[string]any)["strategy"].(map[string]any)
	if ru, _ := strategy["rollingUpdate"].(map[string]any); strategy["type"] != "RollingUpdate" || intAny(ru["maxSurge"]) != 1 || intAny(ru["maxUnavailable"]) != 0 {
		t.Fatalf("strategy: %v", strategy)
	}
	e.at(id, 3, "rolling out")
	e.report(e.router, "r1", map[string]int{id: 50})
	e.at(id, 3, "the load waits for the rollout")
	if !e.settle(id, false) {
		t.Fatal("the rollout must settle")
	}
	if fn := e.fn(id); fn.Image != "nginx:2" || fn.DeployingID != "" {
		t.Fatalf("after the rollout: %+v", fn)
	}
	e.at(id, 3, "rolled out")
	e.report(e.router, "r1", map[string]int{id: 50})
	e.at(id, 5, "and scaled again")
	// A wake (the proxy saw no ready pod for a moment) keeps what runs.
	if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
		t.Fatalf("wake: %d", got)
	}
	e.at(id, 5, "woken while running five")
}

// Two API replicas that get the reports of different proxy replicas at the same moment decide on
// the same sum and do not fight: one count, the same on the record and the cluster, set once.
func TestFunctionScaleTwoAPIReplicas(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"api","image":"nginx:1","maxInstances":5,"concurrency":20}`)
	e.settle(id, false)
	// A second API process: the same database and cluster. Copied field by field (reflect): the
	// one lock it has is the commit poller's, not taken here.
	other := new(Handler)
	reflect.ValueOf(other).Elem().Set(reflect.ValueOf(e.h).Elem())
	r2 := chi.NewRouter()
	other.Routes(r2)
	var scaledAt *time.Time
	for round := range 10 {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); e.report(e.router, "r1", map[string]int{id: 50}) }() // alone: 3
		go func() { defer wg.Done(); e.report(r2, "r2", map[string]int{id: 10}) }()       // alone: 1
		wg.Wait()
		e.report(r2, "r2", map[string]int{id: 10}) // whichever replica skipped a locked decision decides now
		e.at(id, 3, "60 in flight over two proxies, two API replicas")
		fn := e.fn(id)
		if round == 0 {
			scaledAt = fn.ScaledAt
		} else if !fn.ScaledAt.Equal(*scaledAt) {
			t.Fatalf("round %d: the count was changed again (%v, first %v)", round, fn.ScaledAt, scaledAt)
		}
	}
}

// The compare-and-set and the lock themselves. A decision made on a count that changed meanwhile
// (another API replica's scale on load, the reaper, a rollout that claimed the function) is dropped,
// up or down, and the record, the cluster and the billed row keep what the other writer left. A
// way up that is no decision on load (a wake, a deploy) never lowers a count the load has raised.
// A report decides nothing while another replica holds the location's lock.
func TestFunctionScaleConflicts(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"api","image":"nginx:1","maxInstances":5,"concurrency":10}`)
	e.settle(id, false)
	// conflict decides to go to n on the record as it is now, and writes after other() changed it.
	conflict := func(n int, other func(), want int, why string) {
		t.Helper()
		stale := e.fn(id)
		other()
		if err := e.h.rescaleFunction(e.ctx, e.fs(), &stale, n, time.Now().UTC()); err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		e.at(id, want, why)
	}
	wake := func() {
		t.Helper()
		if got := e.proxy(fnWakeToken, id+"/wake", ""); got != 200 {
			t.Fatalf("wake: %d", got)
		}
	}
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 3, "30 in flight")
	conflict(2, func() { e.report(e.router, "r1", map[string]int{id: 50}) }, 5, "a step down from three, after another replica scaled it to five")
	conflict(4, func() { e.idle(id, time.Hour); e.reap() }, 0, "a step down from five, after the reaper stopped it")
	wake()
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 3, "woken, 30 in flight")
	conflict(4, func() { e.report(e.router, "r1", map[string]int{id: 50}) }, 5, "a step up from three, after another replica scaled it to five")

	// A wake in a burst: it read the function at zero, or at the one instance of the first wake,
	// before the first load report took it to four. The four stay, on the cluster and the row.
	for _, woken := range []bool{false, true} {
		e.idle(id, time.Hour)
		e.reap()
		e.at(id, 0, "idle")
		if woken {
			wake()
		}
		stale := e.fn(id)
		if !woken {
			wake()
		}
		e.report(e.router, "r1", map[string]int{id: 40})
		e.at(id, 4, "40 in flight after the wake")
		e.idle(id, time.Minute)
		if err := e.h.scaleFunction(e.ctx, e.fs(), &stale, max(1, stale.Instances)); err != nil {
			t.Fatal(err)
		}
		e.at(id, 4, "a wake that read the function before the load scaled it")
		if fn := e.fn(id); stale.Instances != 4 || fn.LastActiveAt == nil || time.Since(*fn.LastActiveAt) > 30*time.Second {
			t.Fatalf("the wake is told what runs (%d) and is activity (%v)", stale.Instances, fn.LastActiveAt)
		}
	}

	// A rollout holds the function: a decision made before its claim does not change the count,
	// which is still the one it was made on.
	conflict(5, func() {
		if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:2"}`); code != 200 {
			t.Fatalf("deploy: %d", code)
		}
	}, 4, "a step up, after a rollout claimed the function")
	conflict(3, func() {}, 4, "a step down in the middle of a rollout")
	e.settle(id, false)

	// The location's lock, held by another API replica in the middle of its decisions.
	db := e.h.fns.DB()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	unlock := sync.OnceFunc(func() { close(release) })
	defer unlock() // also when the test fails: the lock must be released before the pool closes
	go func() {
		locked, err := db.WithAdvisoryLock(e.ctx, "function-scale:svc", true, func() error {
			close(held)
			<-release
			return nil
		})
		if err == nil && !locked {
			close(held)
			err = errors.New("the lock was not free")
		}
		done <- err
	}()
	<-held
	e.report(e.router, "r1", map[string]int{id: 50})
	e.at(id, 4, "a report while another replica decides")
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.report(e.router, "r1", map[string]int{id: 50})
	e.at(id, 5, "the next report decides")
}

// After a gap in the reports (the API or its database was down, or the path from the proxies) the
// first report is one replica's: the load of the others on record is too old to count, and nobody
// saw the function calm meanwhile. It is not stepped down on that report, and its window starts
// again.
func TestFunctionScaleAfterGap(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"api","image":"nginx:1","maxInstances":4,"concurrency":50}`)
	e.settle(id, false)
	e.report(e.router, "r1", map[string]int{id: 90})
	e.report(e.router, "r2", map[string]int{id: 90})
	e.at(id, 4, "180 in flight over two replicas")
	// 90 s without a report: everything on record is that old.
	gap := time.Now().UTC().Add(-90 * time.Second)
	if _, err := e.h.fns.SetByID(e.ctx, id, pgdoc.M{"scaledAt": gap, "busyAt": gap, "loadAt": gap}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.fnProxies().SetFields(e.ctx, pgdoc.M{"serviceId": "svc"}, pgdoc.M{"at": gap}, nil); err != nil {
		t.Fatal(err)
	}
	e.report(e.router, "r1", map[string]int{id: 90})
	e.at(id, 4, "the first report after the gap: r1's 90 alone would be two instances")
	if fn := e.fn(id); fn.BusyAt == nil || time.Since(*fn.BusyAt) > 30*time.Second {
		t.Fatalf("the window starts again after a gap: busyAt %v", fn.BusyAt)
	}
	e.report(e.router, "r1", map[string]int{id: 90})
	e.at(id, 4, "the report after it, inside the new window")
	e.report(e.router, "r2", map[string]int{id: 90})
	e.at(id, 4, "both replicas again")
	// Reports that keep coming and a load that stays low: one step down a window later, as ever.
	e.scaledAgo(id, 2*functionScaleDownWindow)
	e.report(e.router, "r1", map[string]int{id: 40})
	e.report(e.router, "r2", map[string]int{id: 40})
	e.at(id, 3, "80 in flight a window later")
}

// Instances the cluster cannot place are billed and serve nothing. The scale reconcile notes them
// when it first sees them, and the load adds no more from then on; a minute later it takes them
// off the count, down to the ready ones. The load may ask again after the hold.
func TestFunctionScaleUnplaced(t *testing.T) {
	e := newFnEnv(t)
	id := e.create(`{"name":"api","image":"nginx:1","maxInstances":5,"concurrency":10}`)
	e.settle(id, false)
	e.cluster.room = map[string]int{id: 2}
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 3, "30 in flight: three asked for, two have a node")
	e.sync()
	e.at(id, 3, "first seen: it may be on its way to a node")
	if e.fn(id).UnplacedAt == nil {
		t.Fatal("the instance without a node was not noted")
	}
	e.report(e.router, "r1", map[string]int{id: 50}) // the requests pile up behind it
	e.at(id, 3, "the load adds none while one waits for a node")
	seen := func(d time.Duration) {
		t.Helper()
		if _, err := e.h.fns.SetByID(e.ctx, id, pgdoc.M{"unplacedAt": time.Now().UTC().Add(-d)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	seen(time.Minute)
	e.sync()
	e.at(id, 2, "a minute later: down to the instances that are ready")
	e.report(e.router, "r1", map[string]int{id: 50})
	e.at(id, 2, "and the load does not ask for them again at once")
	e.sync()
	e.at(id, 2, "nothing waits now")
	// A deploy can finish again: no instance is missing.
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:2"}`); code != 200 {
		t.Fatalf("deploy: %d", code)
	}
	if !e.settle(id, false) || e.fn(id).Image != "nginx:2" {
		t.Fatalf("the rollout of the two placed instances: %+v", e.fn(id))
	}
	// After the hold, with room again.
	seen(functionUnplacedHold + time.Second)
	e.cluster.room = nil
	e.report(e.router, "r1", map[string]int{id: 50})
	e.at(id, 5, "after the hold")
	// The cluster refuses the pods of one that runs alone (its quota): nothing to take off.
	single := e.create(`{"name":"one","image":"nginx:1"}`)
	e.settle(single, false)
	e.cluster.unplaced = map[string]bool{single: true}
	_ = e.cluster.SetScale(e.ctx, "", "", "", "fn-p", single, 1)
	e.sync()
	e.at(single, 1, "one instance is the wake's and the reaper's")
}

// A rollout of a function that runs several instances replaces them one after another, each with
// the progress deadline of its own: the poller, and the backstop, give it the time of one
// instance for each, and do not fail (and roll back) a rollout that is still under way after the
// time of one.
func TestFunctionScaleRolloutTime(t *testing.T) {
	if rolloutTime(time.Minute, 0) != time.Minute || rolloutTime(time.Minute, 1) != time.Minute || rolloutTime(time.Minute, 5) != 5*time.Minute {
		t.Fatal("rolloutTime: one bound per instance, at least one")
	}
	e := newFnEnv(t)
	id := e.create(`{"name":"api","image":"nginx:1","maxInstances":3,"concurrency":10}`)
	e.settle(id, false)
	e.report(e.router, "r1", map[string]int{id: 30})
	e.at(id, 3, "30 in flight")
	old := functionRolloutBound
	functionRolloutBound = 400 * time.Millisecond
	t.Cleanup(func() { functionRolloutBound = old })
	e.cluster.unplaced = map[string]bool{id: true} // the new instances are not ready yet
	start := time.Now()
	if code, _ := e.call("owner-p", "PATCH", "/project/p/functions/"+id, `{"image":"nginx:2"}`); code != 200 {
		t.Fatalf("deploy: %d", code)
	}
	// The backstop: long after the time of one instance, not yet that of three.
	if _, err := e.h.fns.SetByID(e.ctx, id, pgdoc.M{"updatedAt": time.Now().UTC().Add(-2 * functionStuckAfter)}, nil); err != nil {
		t.Fatal(err)
	}
	e.sync()
	time.Sleep(time.Until(start.Add(2 * functionRolloutBound)))
	if fn := e.fn(id); fn.DeployingID == "" {
		t.Fatalf("a rollout of three instances was closed after the time of one: %q", fn.LastError)
	}
	// Past the time of three, the poller fails it and the function goes back.
	for deadline := time.Now().Add(10 * time.Second); e.fn(id).DeployingID != ""; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the poller did not close the rollout after the time of three instances")
		}
	}
	if fn, deps := e.fn(id), e.deployments(id); fn.Image != "nginx:1" || deps[0].State != depFailed || !strings.Contains(deps[0].Message, "did not finish in time") {
		t.Fatalf("after the time of three: image %s, deployment %+v", fn.Image, deps[0])
	}
	e.at(id, 3, "rolled back, with its count")
}
