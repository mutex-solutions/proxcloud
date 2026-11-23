package kamajik8s

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

// watchAPI is the watch endpoint of an API server, as a script: request n is answered by step n, and every request
// after the last step by the last one. It keeps what each request asked. What it sends is what kube-apiserver sends:
// one JSON document per event on a chunked answer, bookmarks that carry only a resourceVersion, 410 Gone as the
// answer's status or as an ERROR event, and an end of its own when it ends a stream.
type watchAPI struct {
	mu      sync.Mutex
	paths   []string
	queries []url.Values
	steps   []http.HandlerFunc
}

func (a *watchAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	n := len(a.queries)
	a.paths, a.queries = append(a.paths, r.URL.Path), append(a.queries, r.URL.Query())
	a.mu.Unlock()
	a.steps[min(n, len(a.steps)-1)](w, r)
}

func (a *watchAPI) requests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queries)
}

func (a *watchAPI) query(n int) url.Values {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.queries[n]
}

// event writes one watch event of a Secret with a key in it, and flushes it as the API server does.
func event(w http.ResponseWriter, typ, rv string) {
	fmt.Fprintf(w, `{"type":%q,"object":{"kind":"Secret","apiVersion":"v1","metadata":{"name":"web-tls","namespace":"shop","resourceVersion":%q},"type":"kubernetes.io/tls","data":{"tls.key":"S0VZ"}}}`+"\n", typ, rv)
	w.(http.Flusher).Flush()
}

func bookmark(w http.ResponseWriter, rv string) {
	fmt.Fprintf(w, `{"type":"BOOKMARK","object":{"kind":"Secret","apiVersion":"v1","metadata":{"resourceVersion":%q}}}`+"\n", rv)
	w.(http.Flusher).Flush()
}

// gone is the ERROR event of a resourceVersion the API server no longer has.
func gone(w http.ResponseWriter) {
	fmt.Fprint(w, `{"type":"ERROR","object":{"kind":"Status","apiVersion":"v1","status":"Failure","message":"too old resource version: 5 (90)","reason":"Expired","code":410}}`+"\n")
	w.(http.Flusher).Flush()
}

// hold keeps the stream open and silent until the client goes.
func hold(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

// calls is what a watch told its caller.
type calls struct {
	mu   sync.Mutex
	errs []error
	at   []time.Time
}

func (c *calls) changed(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs, c.at = append(c.errs, err), append(c.at, time.Now())
}

func (c *calls) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.errs)
}

func (c *calls) failures() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []error
	for _, err := range c.errs {
		if err != nil {
			out = append(out, err)
		}
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

// startWatch runs c.watch with short waits until the test ends.
func startWatch(t *testing.T, c *Client, minWait, maxWait time.Duration) *calls {
	t.Helper()
	got := &calls{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watch(ctx, "/api/v1/namespaces/shop/secrets", "metadata.name=web-tls", got.changed, minWait, maxWait)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("the watch did not end with its context")
		}
	})
	return got
}

// An event asks for a turn, a bookmark does not, and the stream after one the server ended starts where that one
// stopped: at the bookmark's resourceVersion, which is newer than the last event's.
func TestWatchEventsBookmarksAndResume(t *testing.T) {
	end := make(chan struct{})
	api := &watchAPI{steps: []http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			event(w, "ADDED", "5")
			bookmark(w, "9")
			select { // then the server's timeout: the handler returns, the answer ends as an answer does
			case <-end:
			case <-r.Context().Done():
			}
		},
		func(w http.ResponseWriter, r *http.Request) {
			event(w, "MODIFIED", "10")
			hold(w, r)
		},
	}}
	got := startWatch(t, testServer(t, api.ServeHTTP), 10*time.Millisecond, 50*time.Millisecond)
	eventually(t, "the ADDED event", func() bool { return got.n() == 1 })
	time.Sleep(100 * time.Millisecond)
	if got.n() != 1 {
		t.Fatalf("a bookmark asked for a turn: %d calls", got.n())
	}
	close(end)
	// The end of the stream is one call, the event of the next stream another.
	eventually(t, "the end and the next stream's event", func() bool { return got.n() == 3 })
	if errs := got.failures(); len(errs) != 0 {
		t.Fatalf("a stream the server ended itself was reported as a failure: %v", errs)
	}
	q := api.query(0)
	if q.Get("watch") != "1" || q.Get("allowWatchBookmarks") != "true" || q.Get("timeoutSeconds") != "300" ||
		q.Get("fieldSelector") != "metadata.name=web-tls" || q.Has("resourceVersion") {
		t.Fatalf("first request = %v", q)
	}
	if rv := api.query(1).Get("resourceVersion"); rv != "9" {
		t.Fatalf("the second stream starts at %q, want the bookmark's 9", rv)
	}
}

// 410 Gone, in both forms the API server has: the next stream starts without a resourceVersion (the server then
// sends every object again), a turn is asked for since events were lost, and it is no failure.
func TestWatchGone(t *testing.T) {
	api := &watchAPI{steps: []http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { event(w, "ADDED", "5") },
		func(w http.ResponseWriter, r *http.Request) { // the watch cache refuses the version at once
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			fmt.Fprint(w, `{"kind":"Status","status":"Failure","reason":"Expired","code":410}`)
		},
		func(w http.ResponseWriter, r *http.Request) { // or it notices in the stream
			event(w, "ADDED", "20")
			gone(w)
		},
		hold,
	}}
	got := startWatch(t, testServer(t, api.ServeHTTP), 10*time.Millisecond, 50*time.Millisecond)
	eventually(t, "four streams", func() bool { return api.requests() == 4 })
	if rv := api.query(1).Get("resourceVersion"); rv != "5" {
		t.Fatalf("stream 2 starts at %q, want 5", rv)
	}
	for _, n := range []int{2, 3} {
		if q := api.query(n); q.Has("resourceVersion") {
			t.Fatalf("stream %d after 410 Gone starts at %q, want none", n+1, q.Get("resourceVersion"))
		}
	}
	// Two events and three ends.
	if got.n() != 5 || len(got.failures()) != 0 {
		t.Fatalf("calls = %d, failures = %v", got.n(), got.failures())
	}
}

// A stream that is cut off (the connection goes, no end of the answer) is a failure that is said, a turn is asked
// for, and the next stream goes on from the last resourceVersion.
func TestWatchDroppedStream(t *testing.T) {
	api := &watchAPI{steps: []http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			event(w, "ADDED", "5")
			panic(http.ErrAbortHandler) // net/http closes the connection in the middle of the answer
		},
		func(w http.ResponseWriter, r *http.Request) {
			event(w, "MODIFIED", "6")
			hold(w, r)
		},
	}}
	got := startWatch(t, testServer(t, api.ServeHTTP), 10*time.Millisecond, 50*time.Millisecond)
	eventually(t, "event, failed end, event", func() bool { return got.n() == 3 })
	if errs := got.failures(); len(errs) != 1 {
		t.Fatalf("failures = %v, want the cut stream's one", errs)
	}
	if rv := api.query(1).Get("resourceVersion"); rv != "5" {
		t.Fatalf("the stream after the cut starts at %q, want 5", rv)
	}
}

// attempts reads how a watch went at a server that never gives it a working stream: how often it tried in d.
func attempts(t *testing.T, got *calls, d time.Duration) (n int, lastGap time.Duration) {
	t.Helper()
	time.Sleep(d)
	got.mu.Lock()
	defer got.mu.Unlock()
	if n = len(got.at); n >= 2 {
		lastGap = got.at[n-1].Sub(got.at[n-2])
	}
	return n, lastGap
}

// An API server that is down is asked again after a wait that doubles up to the longest one, not in a loop: with
// waits of 20 ms to 160 ms that is 8 or 9 attempts in a second, where a constant 20 ms would be 50.
func TestWatchDeadServerBacksOff(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	c := &Client{rc: &restConfig{server: "https://" + addr, tlsCfg: &tls.Config{MinVersion: tls.VersionTLS12}, timeout: time.Second}}
	got := startWatch(t, c, 20*time.Millisecond, 160*time.Millisecond)
	n, lastGap := attempts(t, got, time.Second)
	if n < 4 || n > 12 || lastGap < 120*time.Millisecond {
		t.Fatalf("%d attempts in a second, the last two %v apart: want 8 or 9, 160 ms apart", n, lastGap)
	}
	for _, err := range got.failures() {
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
	}
	if len(got.failures()) != n {
		t.Fatalf("%d of %d attempts were reported as failures", len(got.failures()), n)
	}
}

// No answer of the API server makes the watch ask in a loop: one that ends every stream at once, one that refuses
// the watch (an identity without the verb), and one that says an error in every stream are asked with the growing
// wait, and one that sends an event and hangs up, every time, is asked once per shortest wait.
func TestWatchNoBusyLoop(t *testing.T) {
	for name, step := range map[string]http.HandlerFunc{
		"an empty stream that ends": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
		"forbidden": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"kind":"Status","status":"Failure","message":"secrets \"web-tls\" is forbidden","reason":"Forbidden","code":403}`)
		},
		"an error event": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"type":"ERROR","object":{"kind":"Status","status":"Failure","message":"etcdserver: leader changed","code":500}}`+"\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := &watchAPI{steps: []http.HandlerFunc{step}}
			got := startWatch(t, testServer(t, api.ServeHTTP), 20*time.Millisecond, 160*time.Millisecond)
			if n, lastGap := attempts(t, got, time.Second); n < 4 || n > 12 || lastGap < 120*time.Millisecond {
				t.Fatalf("%d attempts in a second, the last two %v apart: want 8 or 9, 160 ms apart", n, lastGap)
			}
			var ae *APIError
			if errs := got.failures(); name == "forbidden" && (len(errs) == 0 || !errors.As(errs[0], &ae) || ae.Status != http.StatusForbidden) {
				t.Fatalf("failures = %v, want the API server's 403", errs)
			}
		})
	}
	t.Run("an event and the end", func(t *testing.T) {
		api := &watchAPI{steps: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) { event(w, "ADDED", "") }}}
		startWatch(t, testServer(t, api.ServeHTTP), 100*time.Millisecond, time.Second)
		time.Sleep(time.Second)
		if n := api.requests(); n < 5 || n > 11 {
			t.Fatalf("%d streams in a second with 100 ms between two, want about 10", n)
		}
	})
}

// A stream outlives the time limit the client has on every other call: a quiet cluster's stream is silent for
// minutes, and ending it at 30 s would make every watch a failure.
func TestWatchHasNoTimeLimitOnTheStream(t *testing.T) {
	api := &watchAPI{steps: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		event(w, "MODIFIED", "7")
		hold(w, r)
	}}}
	c := testServer(t, api.ServeHTTP)
	c.rc.timeout = 100 * time.Millisecond
	c.http = c.rc.httpClient()
	got := startWatch(t, c, 10*time.Millisecond, 50*time.Millisecond)
	eventually(t, "the event after the silence", func() bool { return got.n() >= 1 })
	if errs := got.failures(); len(errs) != 0 || got.n() != 1 || api.requests() != 1 {
		t.Fatalf("calls = %d, streams = %d, failures = %v: want the one event of the one stream", got.n(), api.requests(), errs)
	}
}

// What each watch asks the API server for: all Services, all Nodes, and of Secrets one name in one namespace.
func TestWatchPaths(t *testing.T) {
	api := &watchAPI{steps: []http.HandlerFunc{func(w http.ResponseWriter, r *http.Request) {
		event(w, "ADDED", "1")
		hold(w, r)
	}}}
	c := testServer(t, api.ServeHTTP)
	for _, tc := range []struct {
		run            func(context.Context, func(error))
		path, selector string
	}{
		{c.WatchServices, "/api/v1/services", ""},
		{c.WatchNodes, "/api/v1/nodes", ""},
		{func(ctx context.Context, changed func(error)) { c.WatchSecret(ctx, "shop", "web-tls", changed) }, "/api/v1/namespaces/shop/secrets", "metadata.name=web-tls"},
	} {
		got := &calls{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); tc.run(ctx, got.changed) }()
		eventually(t, "the event on "+tc.path, func() bool { return got.n() == 1 })
		cancel()
		<-done
		api.mu.Lock()
		path, q := api.paths[len(api.paths)-1], api.queries[len(api.queries)-1]
		api.mu.Unlock()
		if path != tc.path || q.Get("fieldSelector") != tc.selector || q.Get("watch") != "1" {
			t.Fatalf("asked %s %v, want %s with selector %q", path, q, tc.path, tc.selector)
		}
		if got.n() != 1 {
			t.Fatalf("a watch that was ended asked for a turn: %d calls", got.n())
		}
	}
}
