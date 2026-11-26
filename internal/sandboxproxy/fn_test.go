package sandboxproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

const (
	fnDom        = "fn.test"
	fnA          = "fn-00000000000000aa" // host label fnHostA
	fnB          = "fn-00000000000000bb" // host label fnHostB, and fnHostPaused when suspended
	fnHostA      = "app-0a0a0a"
	fnHostB      = "other-0b0b0b"
	fnHostPaused = "paused-0c0c0c"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fnLookup resolves the host labels fnHostA, fnHostB and fnHostPaused to functions listening on
// port.
func fnLookup(port int, n *atomic.Int32) FnLookup {
	return func(_ context.Context, host string) (Fn, error) {
		n.Add(1)
		switch host {
		case fnHostA:
			return Fn{ID: fnA, Namespace: "fn-p1", Port: port}, nil
		case fnHostB:
			return Fn{ID: fnB, Namespace: "fn-p1", Port: port}, nil
		case fnHostPaused:
			return Fn{ID: fnB, Namespace: "fn-p1", Port: port, Suspended: true}, nil
		}
		return Fn{}, ErrNotFound
	}
}

// fnAddr is a pod of f on this host.
func fnAddr(f Fn) []string { return []string{fmt.Sprintf("127.0.0.1:%d", f.Port)} }

func fnLocal(_ context.Context, f Fn) ([]string, error) { return fnAddr(f), nil }

func fnServer(t *testing.T, lookup FnLookup, pods FnPods, wk *Waker) (*httptest.Server, *Activity) {
	t.Helper()
	act := NewActivity()
	s := httptest.NewServer(FunctionHandler(fnDom, lookup, pods, wk, act, quiet))
	t.Cleanup(s.Close)
	return s, act
}

func TestFnUnknownHostIs404WithoutWake(t *testing.T) {
	api, wakes, _ := wakeAPI(t, 200, 0, nil)
	var lookups atomic.Int32
	pods := func(context.Context, Fn) ([]string, error) {
		t.Error("pods were listed for an unknown host")
		return nil, ErrNotFound
	}
	s, act := fnServer(t, fnLookup(80, &lookups), pods, NewWaker(api.URL+"/internal/functions", "tok"))
	for _, host := range []string{
		"nope-0d0d0d." + fnDom, "NOPE-0D0D0D." + fnDom + ":443", // asked of the lookup, which answers from memory
		// Not a function host: never looked up.
		"a.b." + fnDom, fnDom, fnHostA + ".other.test", "a_b-0a0a0a." + fnDom, "a,x=y-0a0a0a." + fnDom, "-a-0a0a0a." + fnDom,
		"app." + fnDom, "app-0a0a0." + fnDom, "app-0a0a0g." + fnDom, "0app-0a0a0a." + fnDom, strings.Repeat("a", 41) + "-0a0a0a." + fnDom,
		"sandbox." + fnDom, "localhost", "127.0.0.1", "8080-sbx-1a2b3c4d." + fnDom, "8080-sbx-1a2b3c4d." + dom,
	} {
		if code, body := get(t, s, host, "/", nil); code != 404 || !strings.Contains(body, "The function was not found") {
			t.Errorf("%s: %d %s", host, code, body)
		}
	}
	if lookups.Load() != 2 || wakes.Load() != 0 || len(takeIDs(act)) != 0 {
		t.Fatalf("lookups = %d, wake calls = %d", lookups.Load(), wakes.Load())
	}
}

func TestFnSuspendedIs503(t *testing.T) {
	api, wakes, _ := wakeAPI(t, 200, 0, nil)
	var lookups atomic.Int32
	pods := func(context.Context, Fn) ([]string, error) {
		t.Error("pods were listed for a suspended function")
		return nil, ErrNotFound
	}
	s, act := fnServer(t, fnLookup(80, &lookups), pods, NewWaker(api.URL, "tok"))
	if code, body := get(t, s, fnHostPaused+"."+fnDom, "/", nil); code != 503 || !strings.Contains(body, "suspended") {
		t.Fatalf("%d %s", code, body)
	}
	if wakes.Load() != 0 || len(takeIDs(act)) != 0 {
		t.Fatalf("a suspended function was woken or reported active (%d wake calls)", wakes.Load())
	}
}

// A cold function: the callers that arrive together share one wake, are held while the pod
// starts, and are all forwarded. A caller that arrives while the pod is starting waits for it
// without a second wake.
func TestFnColdWakesOnceThenForwards(t *testing.T) {
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) }))
	var state atomic.Int32 // 0 no pod, 1 starting, 2 ready
	pods := func(_ context.Context, f Fn) ([]string, error) {
		switch state.Load() {
		case 0:
			return nil, ErrNotFound
		case 1:
			return nil, ErrNotReady
		}
		return fnAddr(f), nil
	}
	api, wakes, bodies := wakeAPI(t, 200, 200*time.Millisecond, func() {
		state.Store(1)
		time.AfterFunc(300*time.Millisecond, func() { state.Store(2) })
	})
	var lookups atomic.Int32
	s, _ := fnServer(t, fnLookup(port, &lookups), pods, NewWaker(api.URL+"/internal/functions", "tok"))

	var wg sync.WaitGroup
	call := func() {
		defer wg.Done()
		if code, body := get(t, s, fnHostA+"."+fnDom, "/", nil); code != 200 || body != "hi" {
			t.Errorf("%d %s", code, body)
		}
	}
	wg.Add(10)
	for range 10 {
		go call()
	}
	time.Sleep(350 * time.Millisecond) // the wake has answered, the pod is still starting
	wg.Add(1)
	call()
	wg.Wait()
	if wakes.Load() != 1 {
		t.Fatalf("wake calls = %d, want 1", wakes.Load())
	}
	if _, ok := bodies.Load("/internal/functions/" + fnA + "/wake"); !ok {
		t.Fatal("the wake did not go to /internal/functions/{id}/wake")
	}
}

func TestFnWarmForwardsToReadyPod(t *testing.T) {
	var got http.Header
	var gotHost string
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotHost = r.Header, r.Host
		_, _ = w.Write([]byte("ok"))
	}))
	api, wakes, _ := wakeAPI(t, 200, 0, nil)
	var lookups, lists atomic.Int32
	pods := func(_ context.Context, f Fn) ([]string, error) {
		lists.Add(1)
		if f.ID != fnA || f.Namespace != "fn-p1" {
			t.Errorf("pods of %+v", f)
		}
		return fnAddr(f), nil
	}
	s, _ := fnServer(t, fnLookup(port, &lookups), pods, NewWaker(api.URL, "tok"))
	host := fnHostA + "." + fnDom
	// What the client says of its own address and scheme is not passed on: the peer and the
	// listener are (fn_edge_test.go has the whole list, over TLS).
	code, body := get(t, s, host, "/", map[string]string{"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "https"})
	if code != 200 || body != "ok" {
		t.Fatalf("%d %s", code, body)
	}
	if gotHost != host || got.Get("X-Forwarded-Host") != host || got.Get("X-Forwarded-Proto") != "http" ||
		got.Get("X-Forwarded-For") != "127.0.0.1" {
		t.Fatalf("host %q, forwarded headers %v", gotHost, got)
	}
	if get(t, s, host, "/", nil); got.Get("X-Forwarded-Proto") != "http" || got.Get("X-Forwarded-For") != "127.0.0.1" {
		t.Fatalf("direct request: forwarded headers %v", got)
	}
	if wakes.Load() != 0 || lists.Load() != 1 {
		t.Fatalf("wake calls = %d, pod lists = %d", wakes.Load(), lists.Load())
	}
}

func TestFnWakeAndPodAnswers(t *testing.T) {
	none := func(context.Context, Fn) ([]string, error) { return nil, ErrNotFound }
	starting := func(context.Context, Fn) ([]string, error) { return nil, ErrNotReady }
	broken := func(context.Context, Fn) ([]string, error) { return nil, errors.New("boom") }
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := l.Addr().(*net.TCPAddr).Port
	l.Close()
	_, openPort := upstream(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for _, tc := range []struct {
		name         string
		pods         FnPods
		port         int
		api          int // the wake route's answer
		want, wakes  int
		retry, inMsg string
	}{
		{"unknown to the api", none, openPort, 404, 404, 1, "", "not found"},
		{"refused", none, openPort, 403, 503, 1, "", "not available"},
		{"no working deployment", none, openPort, 409, 503, 1, "2", "could not be started"},
		{"api down", none, openPort, 500, 503, 1, "2", "could not be started"},
		{"bad wake token", none, openPort, 401, 503, 1, "2", "could not be started"},
		{"woken but no pod in time", none, openPort, 200, 503, 1, "2", "is starting"},
		{"starting, never ready", starting, openPort, 200, 503, 0, "2", "is starting"},
		{"cluster read fails", broken, openPort, 200, 502, 0, "", "lookup failed"},
		{"pod gone", fnLocal, closedPort, 200, 502, 0, "", "not reachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, wakes, _ := wakeAPI(t, tc.api, 0, nil)
			wk := NewWaker(api.URL, "tok")
			wk.Wait = 250 * time.Millisecond
			var lookups atomic.Int32
			s, _ := fnServer(t, fnLookup(tc.port, &lookups), tc.pods, wk)
			req, _ := http.NewRequest("GET", s.URL, nil)
			req.Host = fnHostA + "." + fnDom
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != tc.want || res.Header.Get("Retry-After") != tc.retry || !strings.Contains(string(body), tc.inMsg) ||
				strings.Contains(string(body), "andbox") {
				t.Errorf("got %d retry=%q %s", res.StatusCode, res.Header.Get("Retry-After"), body)
			}
			if wakes.Load() != int32(tc.wakes) {
				t.Errorf("wake calls = %d, want %d", wakes.Load(), tc.wakes)
			}
		})
	}
}

// fnRollout is a function whose only instance is replaced the way a rollout (or a scale-down
// followed by a wake) does it: the proxy has the old instance's address cached, the old instance
// is gone, and the cluster lists the new one. It returns the proxy, the new instance's port, a
// func that replaces the old instance, and the number of pod lists.
func fnRollout(t *testing.T, old, next http.Handler) (s *httptest.Server, replace func(), lists *atomic.Int32) {
	t.Helper()
	oldSrv, oldPort := upstream(t, old)
	_, nextPort := upstream(t, next)
	var replaced atomic.Bool
	lists = new(atomic.Int32)
	pods := func(context.Context, Fn) ([]string, error) {
		lists.Add(1)
		if replaced.Load() {
			return []string{fmt.Sprintf("127.0.0.1:%d", nextPort)}, nil
		}
		return []string{fmt.Sprintf("127.0.0.1:%d", oldPort)}, nil
	}
	var lookups atomic.Int32
	s, _ = fnServer(t, fnLookup(oldPort, &lookups), pods, NewWaker("http://127.0.0.1:1", "tok"))
	return s, func() { oldSrv.Close(); replaced.Store(true) }, lists
}

// An instance that stops while its address is still cached (a rollout stops the old pod as soon
// as the new one is ready) must not cost the client a 502: nothing was sent to it, so the request
// goes to the instance the cluster lists now, body and all.
func TestFnRetriesOnAnotherInstanceWhenTheCachedOneIsGone(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			echo := func(name string) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					b, _ := io.ReadAll(r.Body)
					// No kept-alive connection to the old instance: the call after the rollout
					// must dial, not find out on a connection that is being closed under it.
					w.Header().Set("Connection", "close")
					_, _ = fmt.Fprintf(w, "%s:%s:%s", name, r.Method, b)
				})
			}
			s, replace, lists := fnRollout(t, echo("old"), echo("new"))
			call := func() (int, string) {
				var body io.Reader
				if method == "POST" {
					body = strings.NewReader("payload")
				}
				req, _ := http.NewRequest(method, s.URL, body)
				req.Host = fnHostA + "." + fnDom
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				b, _ := io.ReadAll(res.Body)
				return res.StatusCode, string(b)
			}
			want := ":" + method + ":"
			if method == "POST" {
				want += "payload"
			}
			if code, body := call(); code != 200 || body != "old"+want {
				t.Fatalf("before the rollout: %d %s", code, body)
			}
			replace() // well inside fnCacheTTL: the proxy still has the old address
			if code, body := call(); code != 200 || body != "new"+want {
				t.Fatalf("after the rollout: %d %s", code, body)
			}
			if lists.Load() != 2 {
				t.Fatalf("pod lists = %d, want 2 (the cached one, and one after the dial failed)", lists.Load())
			}
		})
	}
}

// A request that reached an instance is never sent to another one: the instance may have acted on
// it. Here the instance reads the whole request and is gone before it answers. The GET is the
// case a dial error alone does not tell apart: it went out on a kept-alive connection, and the
// transport, which may replay a GET, dials the instance again before it reports the failure.
func TestFnDoesNotRetryARequestThatWasSent(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			var got, second atomic.Int32
			var replace func()
			dies := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/charge" {
					_, _ = io.ReadAll(r.Body)
					got.Add(1)
					c, _, _ := w.(http.Hijacker).Hijack()
					replace() // stops listening first, so the transport's own second dial fails
					c.Close()
				}
			})
			s, replace, lists := fnRollout(t, dies, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { second.Add(1) }))
			if code, body := get(t, s, fnHostA+"."+fnDom, "/", nil); code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			var payload io.Reader
			if method == "POST" {
				payload = strings.NewReader("payload")
			}
			req, _ := http.NewRequest(method, s.URL+"/charge", payload)
			req.Host = fnHostA + "." + fnDom
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 502 || got.Load() != 1 || second.Load() != 0 || lists.Load() != 1 {
				t.Fatalf("%d %s: the instance got the request %d times, another instance %d times, pod lists = %d",
					res.StatusCode, body, got.Load(), second.Load(), lists.Load())
			}
		})
	}
}

// A request body the proxy did not read must not be taken for the next request on the client's
// connection. The server closes the connection after such an answer, and it can only tell from
// the Body of the request it handed out: the proxy must leave that one alone.
func TestFnUnreadBodyIsNotTheNextRequest(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := l.Addr().(*net.TCPAddr).Port
	l.Close()
	var lookups atomic.Int32
	s, _ := fnServer(t, fnLookup(closedPort, &lookups), fnLocal, NewWaker("http://127.0.0.1:1", "tok"))
	c, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	// More than the 256 KB a server reads on its own to reuse a connection.
	body := "GET / HTTP/1.1\r\nHost: nope-0d0d0d." + fnDom + "\r\n\r\n" + strings.Repeat("A", 400<<10)
	go fmt.Fprintf(c, "POST / HTTP/1.1\r\nHost: %s.%s\r\nContent-Length: %d\r\n\r\n%s", fnHostA, fnDom, len(body), body)
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != 502 || !res.Close {
		t.Fatalf("%d, connection close = %v", res.StatusCode, res.Close)
	}
	if res, err = http.ReadResponse(br, nil); err == nil {
		t.Fatalf("the unread body was answered as a request: %d", res.StatusCode)
	}
}

// One retry, not a loop: when the instance listed after the failure is unreachable too, the
// answer is the 502.
func TestFnRetriesOnlyOnce(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closedPort := l.Addr().(*net.TCPAddr).Port
	l.Close()
	var lookups, lists atomic.Int32
	pods := func(_ context.Context, f Fn) ([]string, error) {
		lists.Add(1)
		return fnAddr(f), nil
	}
	s, _ := fnServer(t, fnLookup(closedPort, &lookups), pods, NewWaker("http://127.0.0.1:1", "tok"))
	if code, body := get(t, s, fnHostA+"."+fnDom, "/", nil); code != 502 || !strings.Contains(body, "not reachable") {
		t.Fatalf("%d %s", code, body)
	}
	if lists.Load() != 2 {
		t.Fatalf("pod lists = %d, want 2", lists.Load())
	}
}

// activityAPI fakes the activity route: it records the ids of every report (and "partial" after
// them for one that says it is not all the proxy has) and fails the first.
func activityAPI(t *testing.T) (*Waker, func() [][]string) {
	t.Helper()
	var mu sync.Mutex
	var reports [][]string
	host, _ := os.Hostname()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			IDs     []string `json:"ids"`
			Every   int      `json:"every"`
			Replica string   `json:"replica"`
			Partial bool     `json:"partial"`
		}
		if r.URL.Path != "/internal/functions/activity" || r.Header.Get("Authorization") != "Bearer tok" ||
			json.NewDecoder(r.Body).Decode(&b) != nil {
			t.Errorf("bad report: %s %s", r.Method, r.URL.Path)
		}
		// The portal takes an instance for idle by the period it is told: it must be the real one.
		if b.Every != int(FnActivityEvery/time.Second) || b.Every < 1 {
			t.Errorf("report names a period of %d s, want %s", b.Every, FnActivityEvery)
		}
		// And by the replica that reports: what one replica leaves out another may be serving.
		if b.Replica == "" || b.Replica != host || b.IDs == nil {
			t.Errorf("report of replica %q with ids %v, want the host name %q and a list", b.Replica, b.IDs, host)
		}
		slices.Sort(b.IDs)
		if b.Partial {
			b.IDs = append(b.IDs, "partial")
		}
		mu.Lock()
		reports = append(reports, b.IDs)
		first := len(reports) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(s.Close)
	return NewWaker(s.URL+"/internal/functions", "tok"), func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reports)
	}
}

func idle(a *Activity) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.inflight) == 0
}

func waitIdle(t *testing.T, a *Activity) {
	t.Helper()
	for range 400 {
		if idle(a) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("requests still counted in flight")
}

func TestFnActivityReport(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	_, port := upstream(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-release
		}
	}))
	var lookups atomic.Int32
	s, act := fnServer(t, fnLookup(port, &lookups), fnLocal, NewWaker("http://127.0.0.1:1", "tok"))
	wk, reports := activityAPI(t)
	ctx := context.Background()

	done := make(chan struct{})
	go func() { defer close(done); get(t, s, fnHostA+"."+fnDom, "/slow", nil) }()
	<-started                              // fnA has a request in flight
	get(t, s, fnHostB+"."+fnDom, "/", nil) // fnB had a request, now finished
	get(t, s, "missing."+fnDom, "/", nil)  // not a function
	for !idleExcept(act, fnA) {
		time.Sleep(5 * time.Millisecond)
	}

	act.Report(ctx, wk, quiet) // refused by the portal: must be sent again
	act.Report(ctx, wk, quiet)
	act.Report(ctx, wk, quiet) // fnB is quiet now, fnA is still in flight
	close(release)
	<-done
	waitIdle(t, act)
	act.Report(ctx, wk, quiet) // nothing has traffic: that is reported too, it is what says they are idle
	want := [][]string{{fnA, fnB}, {fnA, fnB}, {fnA}, {}}
	if got := reports(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("reports %v, want %v", got, want)
	}

	// Run reports on its own, every period, and stops with its context.
	rctx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { defer close(stopped); act.Run(rctx, wk, 10*time.Millisecond, quiet) }()
	get(t, s, fnHostB+"."+fnDom, "/", nil)
	reported := func(want []string) bool {
		return slices.ContainsFunc(reports()[4:], func(r []string) bool { return slices.Equal(r, want) })
	}
	for i := 0; !reported([]string{fnB}) || !reported(nil); i++ {
		if i > 400 {
			t.Fatalf("Run did not report the function, or the periods without one: %v", reports()[4:])
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-stopped
}

// A report that hangs is given up within its timeout, well inside the period: the portal hears of
// the functions with the next one, not a whole report timeout later.
func TestFnActivityReportTimeout(t *testing.T) {
	old := fnActivityTimeout
	fnActivityTimeout = 50 * time.Millisecond
	defer func() { fnActivityTimeout = old }()
	if old <= 0 || old > FnActivityEvery/2 {
		t.Fatalf("a report may take %s, more than half the period of %s", old, FnActivityEvery)
	}
	var calls atomic.Int32
	hang := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-hang
		}
		w.WriteHeader(204)
	}))
	defer api.Close()
	defer close(hang)
	wk, act := NewWaker(api.URL, "tok"), NewActivity()
	act.begin(fnA)
	act.end(fnA)
	start := time.Now()
	act.Report(context.Background(), wk, quiet)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a report that hangs took %s", took)
	}
	if ids, _ := act.take(); !slices.Equal(ids, []string{fnA}) {
		t.Fatalf("after a report that hung the next one carries %v, want the function again", ids)
	}
}

// The first request after a quiet spell is reported at once, not at the next periodic report:
// the reaper decides on lastActiveAt alone, and would stop the instance under that request.
func TestFnActivityFirstRequestReportsAtOnce(t *testing.T) {
	var mu sync.Mutex
	var reports [][]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			IDs []string `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		reports = append(reports, b.IDs)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer api.Close()
	got := func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reports)
	}
	wk := NewWaker(api.URL, "tok")
	act := NewActivity()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go act.Run(ctx, wk, time.Hour, quiet) // the periodic report never comes in this test
	wait := func(n int) {
		t.Helper()
		for i := 0; len(got()) != n; i++ {
			if i > 400 {
				t.Fatalf("reports %v, want %d", got(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	act.begin(fnA)
	wait(1)
	for range 5 { // more requests in the same period say nothing new
		act.begin(fnA)
		act.end(fnA)
	}
	act.end(fnA)
	act.Report(ctx, wk, quiet) // the periodic report: fnA was active
	wait(2)
	act.begin(fnA) // reported less than a period ago: the next periodic report is soon enough
	act.end(fnA)
	act.Report(ctx, wk, quiet)
	act.Report(ctx, wk, quiet) // a period without a request: an empty report
	wait(4)
	act.begin(fnA) // quiet for a whole period: at once again
	act.end(fnA)
	wait(5)
	time.Sleep(50 * time.Millisecond)
	if want := [][]string{{fnA}, {fnA}, {fnA}, {}, {fnA}}; !slices.EqualFunc(got(), want, slices.Equal[[]string]) {
		t.Fatalf("reports %v, want %v", got(), want)
	}
}

func idleExcept(a *Activity, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.inflight) == 1 && a.inflight[id] == 1
}

func TestFnActivityBatches(t *testing.T) {
	var sizes []int
	var partial []bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			IDs     []string `json:"ids"`
			Partial bool     `json:"partial"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		sizes, partial = append(sizes, len(b.IDs)), append(partial, b.Partial)
		w.WriteHeader(204)
	}))
	defer api.Close()
	act := NewActivity()
	for i := range fnActivityBatch + 5 {
		id := fmt.Sprintf("fn-%016x", i)
		act.begin(id)
		act.end(id)
	}
	act.Report(context.Background(), NewWaker(api.URL, "tok"), quiet)
	// Only the request that ends the report says "this is all".
	if !slices.Equal(sizes, []int{fnActivityBatch, 5}) || !slices.Equal(partial, []bool{true, false}) {
		t.Fatalf("report sizes %v, partial %v", sizes, partial)
	}
}

func TestFnWebSocketUpgrade(t *testing.T) {
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "no upgrade", 400)
			return
		}
		c, rw, _ := w.(http.Hijacker).Hijack()
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo:" + line)
		rw.Flush()
	}))
	var lookups atomic.Int32
	s, act := fnServer(t, fnLookup(port, &lookups), fnLocal, NewWaker("http://127.0.0.1:1", "tok"))
	c, err := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s.%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", fnHostA, fnDom)
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("status %q", status)
	}
	for {
		l, _ := br.ReadString('\n')
		if l == "\r\n" || l == "" {
			break
		}
	}
	// An open socket is a request in flight: it is reported for as long as it lasts.
	act.take()
	if ids, _ := act.take(); !slices.Equal(ids, []string{fnA}) {
		t.Fatalf("open websocket not reported as active: %v", ids)
	}
	fmt.Fprint(c, "hello\n")
	if got, _ := br.ReadString('\n'); got != "echo:hello\n" {
		t.Fatalf("got %q", got)
	}
	c.Close()
	waitIdle(t, act)
}

type fnFakeLister struct {
	deployments, pods []map[string]any
	asked             *[]string
}

func (f fnFakeLister) ListCRsAllNamespaces(_ context.Context, group, version, plural, sel string) ([]map[string]any, error) {
	*f.asked = append(*f.asked, group+"/"+version+"/"+plural+"?"+sel)
	return f.deployments, nil
}

func (f fnFakeLister) ListPods(_ context.Context, ns, sel string) ([]map[string]any, error) {
	*f.asked = append(*f.asked, "pods/"+ns+"?"+sel)
	return f.pods, nil
}

// fnDeployment is a function's Deployment as the function package writes it: the names of the
// plan's table (docs/managed-functions.md).
func fnDeployment(ns, name, hostLabel, port string) map[string]any {
	return map[string]any{"metadata": map[string]any{
		"name": name, "namespace": ns,
		"labels": map[string]any{sandbox.LabelManagedBy: sandbox.ManagedByValue,
			"proxcloud.io/function-id": name, "proxcloud.io/function-host": hostLabel},
		"annotations": map[string]any{"proxcloud.io/function-port": port},
	}}
}

func TestK8sFnLookup(t *testing.T) {
	var asked []string
	one := func(items ...map[string]any) FnLookup {
		return K8sFnLookup(fnFakeLister{deployments: items, asked: &asked})
	}
	dep := func() map[string]any { return fnDeployment("fn-p1", fnA, fnHostA, "3000") }
	got, err := one(dep())(context.Background(), fnHostA)
	if want := (Fn{ID: fnA, Namespace: "fn-p1", Port: 3000}); err != nil || got != want {
		t.Fatalf("%+v %v", got, err)
	}
	// Nothing of the host is in the selector: one list answers every host.
	if want := "apps/v1/deployments?app.kubernetes.io/managed-by=proxcloud,proxcloud.io/function-host"; asked[0] != want {
		t.Fatalf("listed %q, want %q", asked[0], want)
	}
	suspended := dep()
	suspended["metadata"].(map[string]any)["annotations"].(map[string]any)["proxcloud.io/function-suspended"] = "true"
	if got, _ = one(suspended)(context.Background(), fnHostA); !got.Suspended {
		t.Fatal("suspended annotation not read")
	}

	// Anything that is not a function Deployment in a function namespace is not a route.
	foreign := fnDeployment("sb-p1", fnA, fnHostA, "3000")
	badName := fnDeployment("fn-p1", "web", fnHostA, "3000")
	noPort := fnDeployment("fn-p1", fnA, fnHostA, "")
	deleting := dep()
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-16T00:00:00Z"
	otherHost := fnDeployment("fn-p1", fnA, fnHostB, "3000")
	for _, items := range [][]map[string]any{nil, {foreign}, {badName}, {noPort}, {deleting}, {otherHost}} {
		if _, err := one(items...)(context.Background(), fnHostA); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v: %v", items, err)
		}
	}
}

func TestK8sFnPods(t *testing.T) {
	pod := func(phase, ip string, ready bool) map[string]any {
		st := "False"
		if ready {
			st = "True"
		}
		return map[string]any{"metadata": map[string]any{"annotations": map[string]any{}}, "status": map[string]any{"phase": phase, "podIP": ip,
			"conditions": []any{map[string]any{"type": "Ready", "status": st}}}}
	}
	leaving := pod("Running", "10.1.1.9", true)
	leaving["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-16T00:00:00Z"
	fn := Fn{ID: fnA, Namespace: "fn-p1", Port: 8080}
	var asked []string
	list := func(pods ...map[string]any) ([]string, error) {
		return K8sFnPods(fnFakeLister{pods: pods, asked: &asked})(context.Background(), fn)
	}

	// A pod is reached on the port of its own snapshot: during a rollout that changes the port
	// the old pod still listens on the old one. Without the annotation, the Deployment's port.
	old := pod("Running", "10.1.1.3", true)
	old["metadata"].(map[string]any)["annotations"].(map[string]any)[fnAnnotationPort] = "3000"
	ips, err := list(pod("Pending", "", false), pod("Running", "10.1.1.2", false), leaving, pod("Running", "10.1.1.1", true), old)
	if err != nil || !slices.Equal(ips, []string{"10.1.1.1:8080", "10.1.1.3:3000"}) {
		t.Fatalf("%v %v", ips, err)
	}
	if want := "pods/fn-p1?proxcloud.io/function-id=" + fnA; asked[0] != want {
		t.Fatalf("listed %q, want %q", asked[0], want)
	}
	for _, only := range []map[string]any{pod("Pending", "", false), pod("Running", "10.1.1.2", false)} {
		if _, err := list(only); !errors.Is(err, ErrNotReady) {
			t.Fatalf("a starting pod means wait, not wake: %v", err)
		}
	}
	// Scaled to zero with the old pod still going away, or a pod that ended: wake.
	for _, pods := range [][]map[string]any{nil, {leaving}, {pod("Failed", "10.1.1.4", false)}} {
		if _, err := list(pods...); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v: %v", pods, err)
		}
	}
}

func TestFnCache(t *testing.T) {
	c := &fnCache[int]{okTTL: time.Hour, missTTL: 20 * time.Millisecond}
	var reads atomic.Int32
	var next error
	read := func(ctx context.Context) (int, error) {
		reads.Add(1)
		time.Sleep(20 * time.Millisecond)
		if ctx.Err() != nil {
			return 0, ctx.Err() // the read must not die with the caller that started it
		}
		return 7, next
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Callers that miss together share one read.
	next = ErrNotReady
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := c.get(ctx, "a", read); !errors.Is(err, ErrNotReady) {
				t.Errorf("got %v", err)
			}
		})
	}
	wg.Wait()
	if reads.Load() != 1 {
		t.Fatalf("%d reads for 20 concurrent callers", reads.Load())
	}
	// "Not ready" is remembered for missTTL only, a result for okTTL.
	time.Sleep(30 * time.Millisecond)
	next = nil
	for range 3 {
		if v, err := c.get(ctx, "a", read); v != 7 || err != nil {
			t.Fatalf("%d %v", v, err)
		}
	}
	if reads.Load() != 2 {
		t.Fatalf("reads = %d, want 2", reads.Load())
	}
	c.forget("a")
	c.get(ctx, "a", read)
	// Other errors are not remembered.
	next = errors.New("boom")
	c.get(ctx, "b", read)
	c.get(ctx, "b", read)
	if reads.Load() != 5 {
		t.Fatalf("reads = %d, want 5", reads.Load())
	}
}

func takeIDs(a *Activity) []string {
	ids, _ := a.take()
	return ids
}

// A function with several ready instances gets an equal share of its requests on each, and no
// instance is refused one (there is no per-instance limit in the proxy).
func TestFnSpreadsRequestsOverInstances(t *testing.T) {
	var hits [3]atomic.Int32
	var addrs []string
	for i := range hits {
		_, port := upstream(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits[i].Add(1) }))
		addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", port))
	}
	var lookups atomic.Int32
	pods := func(context.Context, Fn) ([]string, error) { return addrs, nil }
	s, _ := fnServer(t, fnLookup(1, &lookups), pods, NewWaker("http://127.0.0.1:1", "tok"))
	const n = 900
	for range n {
		if code, _ := get(t, s, fnHostA+"."+fnDom, "/", nil); code != 200 {
			t.Fatalf("status %d", code)
		}
	}
	// 300 each on average; 200..400 is more than 6 standard deviations (≈14) either way.
	for i := range hits {
		if got := hits[i].Load(); got < 200 || got > 400 {
			t.Errorf("instance %d got %d of %d requests: not an equal share", i, got, n)
		}
	}
}

// The periodic report carries each function's load in its period: the most requests in flight at
// once and how many started. A request still in flight counts in the next period too; a function
// without requests in a period is not in its load; a partial report carries none.
func TestFnActivityLoad(t *testing.T) {
	var mu sync.Mutex
	type report struct {
		IDs     []string          `json:"ids"`
		Partial bool              `json:"partial"`
		Load    map[string]fnLoad `json:"load"`
	}
	var got []report
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b report
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer api.Close()
	wk, act := NewWaker(api.URL, "tok"), NewActivity()
	for range 3 { // three at once on fnA, then two more one after the other
		act.begin(fnA)
	}
	act.end(fnA)
	act.end(fnA)
	act.begin(fnA)
	act.end(fnA)
	act.begin(fnB)
	act.end(fnB)
	act.Report(context.Background(), wk, quiet) // fnA keeps one in flight
	act.Report(context.Background(), wk, quiet)
	act.end(fnA)
	act.Report(context.Background(), wk, quiet)
	if err := wk.reportActivity(context.Background(), "r", []string{fnA}, true, nil); err != nil {
		t.Fatal(err)
	}
	want := []map[string]fnLoad{
		{fnA: {Peak: 3, Requests: 4}, fnB: {Peak: 1, Requests: 1}},
		{fnA: {Peak: 1, Requests: 0}},
		{fnA: {Peak: 1, Requests: 0}}, // it ended in this period: it was in flight in it
		nil,
	}
	if len(got) != len(want) {
		t.Fatalf("reports %+v", got)
	}
	for i := range want {
		if len(got[i].Load) != len(want[i]) || (want[i] == nil) != got[i].Partial {
			t.Fatalf("report %d: %+v, want load %v", i, got[i], want[i])
		}
		for id, l := range want[i] {
			if got[i].Load[id] != l {
				t.Errorf("report %d: load of %s %+v, want %+v", i, id, got[i].Load[id], l)
			}
		}
	}
	if act.Report(context.Background(), wk, quiet); len(got[4].Load) != 0 || got[4].Load == nil {
		t.Errorf("a period without requests: load %v, want an empty one", got[4].Load)
	}
	// The last report of a replica that shuts down: what had traffic is still stamped, and its
	// load is cleared (complete, with an empty load), whatever the period held.
	for range 3 {
		act.begin(fnB)
		act.end(fnB)
	}
	if act.Stop(context.Background(), wk, quiet); len(got) != 6 || got[5].Partial || len(got[5].IDs) != 1 || got[5].IDs[0] != fnB || got[5].Load == nil || len(got[5].Load) != 0 {
		t.Errorf("the last report: %+v, want fnB, complete, with an empty load", got[len(got)-1])
	}
}
