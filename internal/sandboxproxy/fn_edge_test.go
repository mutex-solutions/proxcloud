package sandboxproxy

// What function mode owes to being the edge, in one process with the sandbox path: the routing
// rule, the one list, the bounds on held requests, and what an instance is told of the client.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sandboxAndFunctions is the handler of a proxy in function mode, both under dom: a sandbox host
// reaches the port it names on this host, the function fnHostA reaches fnPort. It counts the
// function lookups.
func sandboxAndFunctions(t *testing.T, fnPort int, lookups *atomic.Int32) (both, sandboxOnly http.Handler) {
	t.Helper()
	sb := func(_ context.Context, id string) (Pod, error) {
		if id == "sbx-00000000" {
			return Pod{}, ErrNotFound
		}
		return Pod{IP: "127.0.0.1"}, nil
	}
	sandboxOnly = Handler(dom, sb, quiet)
	fn := FunctionHandler(dom, fnLookup(fnPort, lookups), fnLocal, NewWaker("http://127.0.0.1:1", "tok"), NewActivity(), quiet)
	return WithFunctions(dom, fn, sandboxOnly), sandboxOnly
}

// A sandbox host and a function host through one listener that ends TLS. The function instance is
// told the peer's address and the scheme the listener served, whatever the client claimed.
func TestOneTLSListenerServesSandboxAndFunctionHosts(t *testing.T) {
	_, sbPort := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("sandbox")) }))
	var got http.Header
	_, fnPort := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header
		_, _ = w.Write([]byte("function"))
	}))
	var lookups atomic.Int32
	both, _ := sandboxAndFunctions(t, fnPort, &lookups)
	s := httptest.NewTLSServer(both)
	t.Cleanup(s.Close)
	call := func(host string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("GET", s.URL, nil)
		req.Host = host
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	if code, body := call(fmt.Sprintf("%d-sbx-12345678.%s", sbPort, dom), nil); code != 200 || body != "sandbox" || lookups.Load() != 0 {
		t.Fatalf("sandbox host: %d %s, %d function lookups", code, body, lookups.Load())
	}
	host := fnHostA + "." + dom
	claimed := map[string]string{
		"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "http", "X-Forwarded-Host": "evil.test",
		"Forwarded": "for=203.0.113.9;proto=http", "X-Forwarded-Port": "80", "X-Forwarded-Client-Cert": "x",
		"X-Real-Ip": "203.0.113.9", "True-Client-Ip": "203.0.113.9", "Cf-Connecting-Ip": "203.0.113.9",
		// The same names with an underscore: a CGI-style server hands both spellings to the code as one variable.
		"X_Forwarded_For": "203.0.113.9", "X_Real_Ip": "203.0.113.9", "True_Client_Ip": "203.0.113.9", "Cf_Connecting_Ip": "203.0.113.9",
		"X-Custom": "kept", "X_Custom": "kept",
	}
	if code, body := call(host, claimed); code != 200 || body != "function" {
		t.Fatalf("function host: %d %s", code, body)
	}
	for h, want := range map[string]string{
		"X-Forwarded-For": "127.0.0.1", "X-Forwarded-Proto": "https", "X-Forwarded-Host": host, "X-Custom": "kept",
		"Forwarded": "", "X-Forwarded-Port": "", "X-Forwarded-Client-Cert": "", "X-Real-Ip": "", "True-Client-Ip": "", "Cf-Connecting-Ip": "",
		"X_Forwarded_For": "", "X_Real_Ip": "", "True_Client_Ip": "", "Cf_Connecting_Ip": "", "X_Custom": "kept",
	} {
		if v := strings.Join(got.Values(h), ", "); v != want {
			t.Errorf("the instance got %s: %q, want %q", h, v, want)
		}
	}
}

// With function mode on, every request that is not for a function host is answered exactly as
// the sandbox handler alone answers it, and nothing is looked up for it: sandbox.<domain> and
// localhost with the routing headers still reach a sandbox.
func TestNonFunctionHostsTakeTheSandboxPathUnchanged(t *testing.T) {
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("sandbox " + r.URL.Path)) }))
	var lookups atomic.Int32
	both, sandboxOnly := sandboxAndFunctions(t, 1, &lookups)
	routing := map[string]string{headerSandboxID: "sbx-12345678", headerSandboxPort: fmt.Sprint(port)}
	answer := func(h http.Handler, host string, hdr map[string]string) string {
		r := httptest.NewRequest("GET", "http://"+host+"/x", nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return fmt.Sprint(w.Code, " ", w.Body.String())
	}
	for _, tc := range []struct {
		host string
		hdr  map[string]string
		want string // "" = whatever the sandbox handler says
	}{
		{fmt.Sprintf("%d-sbx-12345678.%s", port, dom), nil, "200 sandbox /x"},
		{"sandbox." + dom, routing, "200 sandbox /x"},
		{"localhost", routing, "200 sandbox /x"},
		{"127.0.0.1", routing, "200 sandbox /x"},
		{"sandbox." + dom, nil, ""},
		{"localhost", nil, ""},
		{fmt.Sprintf("%d-sbx-00000000.%s", port, dom), nil, ""},
		{dom, nil, ""},
		{"app." + dom, nil, ""},                  // no suffix: not a function host
		{fnHostA + ".other.test", nil, ""},       // not this domain
		{fnHostA + "." + dom + ".", nil, ""},     // nor this
		{"a." + fnHostA + "." + dom, nil, ""},    // nor a name below a function host
		{"8080-" + fnHostA + "." + dom, nil, ""}, // a label that starts with a digit is a sandbox's
	} {
		got, was := answer(both, tc.host, tc.hdr), answer(sandboxOnly, tc.host, tc.hdr)
		if got != was || (tc.want != "" && got != tc.want) {
			t.Errorf("%s: %q with function mode, %q without, want %q", tc.host, got, was, tc.want)
		}
	}
	if lookups.Load() != 0 {
		t.Fatalf("%d function lookups for hosts that are not function hosts", lookups.Load())
	}
	// And the function host itself, which the sandbox handler alone refuses, as before the mode existed.
	if got := answer(sandboxOnly, fnHostA+"."+dom, nil); !strings.HasPrefix(got, "400 ") || !strings.Contains(got, "invalid sandbox port") {
		t.Fatalf("a function host with the mode off: %q", got)
	}
	if got := answer(both, strings.ToUpper(fnHostA)+"."+dom+":443", nil); !strings.HasPrefix(got, "502 ") || lookups.Load() != 1 {
		t.Fatalf("a function host with the mode on went to %q (%d lookups), want its instance (port 1: 502)", got, lookups.Load())
	}
}

type fnCountingLister struct {
	deployments   []map[string]any
	lists, podsOf atomic.Int32
}

func (f *fnCountingLister) ListCRsAllNamespaces(context.Context, string, string, string, string) ([]map[string]any, error) {
	f.lists.Add(1)
	return f.deployments, nil
}

func (f *fnCountingLister) ListPods(context.Context, string, string) ([]map[string]any, error) {
	f.podsOf.Add(1)
	return nil, nil
}

// Anyone can ask the proxy for any number of names. Ten thousand that nobody has cost the cluster
// one list per fnCacheTTL, no wake, and leave nothing behind in the proxy.
func TestMadeUpHostsCostOneList(t *testing.T) {
	api, wakes, _ := wakeAPI(t, 200, 0, nil)
	c := &fnCountingLister{deployments: []map[string]any{fnDeployment("fn-p1", fnA, fnHostA, "3000")}}
	act, held, wk := NewActivity(), &fnHeld{}, NewWaker(api.URL, "tok")
	h := functionHandler(fnDom, K8sFnLookup(c), K8sFnPods(c), wk, act, held, quiet)
	start := time.Now()
	var wg sync.WaitGroup
	for g := range 10 {
		wg.Go(func() {
			for i := range 1000 {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("http://made-up-%d-%06x.%s/", g, i, fnDom), nil))
				if w.Code != 404 {
					t.Errorf("%d %s", w.Code, w.Body)
					return
				}
			}
		})
	}
	wg.Wait()
	if most := int32(1 + time.Since(start)/fnCacheTTL); c.lists.Load() < 1 || c.lists.Load() > most {
		t.Fatalf("%d lists for 10,000 made-up hosts in %s, want at most %d", c.lists.Load(), time.Since(start), most)
	}
	wk.mu.Lock()
	remembered := len(wk.notFound)
	wk.mu.Unlock()
	if c.podsOf.Load() != 0 || wakes.Load() != 0 || len(takeIDs(act)) != 0 || held.all != 0 || remembered != 0 {
		t.Fatalf("made-up hosts cost %d pod lists, %d wakes, and left %d wake answers behind", c.podsOf.Load(), wakes.Load(), remembered)
	}
}

// The requests held for an instance are bounded per function and per replica. One over the bound
// is answered 503 with Retry-After at once, not parked; a request that needs no hold is served.
func TestHeldRequestsAreBounded(t *testing.T) {
	_, port := upstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) }))
	// Every host label is a function of its own; only "warm-…" has an instance.
	lookup := func(_ context.Context, host string) (Fn, error) {
		var n int
		if _, err := fmt.Sscanf(host, "cold-%06x", &n); err != nil && !strings.HasPrefix(host, "warm-") {
			return Fn{}, ErrNotFound
		}
		return Fn{ID: fmt.Sprintf("fn-%016x", n), Namespace: "fn-p1", Port: port, Suspended: false}, nil
	}
	pods := func(_ context.Context, f Fn) ([]string, error) {
		if f.ID == fmt.Sprintf("fn-%016x", 0) {
			return fnAddr(f), nil
		}
		return nil, ErrNotReady // starting, and never ready
	}
	held, act := &fnHeld{}, NewActivity()
	wk := NewWaker("http://127.0.0.1:1", "tok")
	wk.Wait = 20 * time.Second
	h := functionHandler(fnDom, lookup, pods, wk, act, held, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	hold := func(fn, n int) {
		for range n {
			wg.Go(func() {
				r := httptest.NewRequest("GET", fmt.Sprintf("http://cold-%06x.%s/", fn, fnDom), nil).WithContext(ctx)
				h.ServeHTTP(httptest.NewRecorder(), r)
			})
		}
	}
	count := func() (all, of1 int) {
		held.mu.Lock()
		defer held.mu.Unlock()
		return held.all, held.per[fmt.Sprintf("fn-%016x", 1)]
	}
	waitHeld := func(want int) {
		t.Helper()
		for i := 0; ; i++ {
			if all, _ := count(); all == want {
				return
			} else if i > 1000 {
				t.Fatalf("%d requests held, want %d", all, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	now := func(host string) (int, string, string, time.Duration) {
		start := time.Now()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://"+host+"."+fnDom+"/", nil))
		return w.Code, w.Header().Get("Retry-After"), w.Body.String(), time.Since(start)
	}

	// One function at its bound: the next request for it is refused at once, another function's is held.
	hold(1, heldPerFunction)
	waitHeld(heldPerFunction)
	if code, retry, body, took := now("cold-000001"); code != 503 || retry != "2" || !strings.Contains(body, "Too many requests are waiting") || took > 5*time.Second {
		t.Fatalf("over the bound of a function: %d retry=%q %s after %s", code, retry, body, took)
	}
	if all, of1 := count(); all != heldPerFunction || of1 != heldPerFunction {
		t.Fatalf("the refused request was counted: %d held, %d for the function", all, of1)
	}
	// The replica at its bound: a function nobody waits for is refused too.
	for fn := 2; fn <= heldPerReplica/heldPerFunction; fn++ {
		hold(fn, heldPerFunction)
	}
	waitHeld(heldPerReplica)
	if code, retry, body, took := now("cold-0000ff"); code != 503 || retry != "2" || !strings.Contains(body, "Too many requests are waiting") || took > 5*time.Second {
		t.Fatalf("over the bound of the replica: %d retry=%q %s after %s", code, retry, body, took)
	}
	// A function with a ready instance needs no place: it is served.
	if code, _, body, _ := now("warm-000000"); code != 200 || body != "hi" {
		t.Fatalf("a warm function while the replica holds its bound: %d %s", code, body)
	}
	// The callers go away: every place is given back.
	cancel()
	wg.Wait()
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.all != 0 || len(held.per) != 0 {
		t.Fatalf("after the held requests ended: %d held, %d functions counted", held.all, len(held.per))
	}
}

// A place is for waiting, not for being served: a request that was held and then reached its
// instance gives the place back, however long the instance takes to answer (a stream, a WebSocket).
func TestServedRequestHoldsNoPlace(t *testing.T) {
	serving, finish := make(chan struct{}), make(chan struct{})
	_, port := upstream(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(serving)
		<-finish
	}))
	finished := sync.OnceFunc(func() { close(finish) })
	t.Cleanup(finished) // before the instance's own cleanup, which waits for its request
	var ready atomic.Bool
	pods := func(_ context.Context, f Fn) ([]string, error) {
		if !ready.Load() {
			return nil, ErrNotReady
		}
		return fnAddr(f), nil
	}
	var lookups atomic.Int32
	held := &fnHeld{}
	h := functionHandler(fnDom, fnLookup(port, &lookups), pods, NewWaker("http://127.0.0.1:1", "tok"), NewActivity(), held, quiet)
	done := make(chan int)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://"+fnHostA+"."+fnDom+"/", nil))
		done <- w.Code
	}()
	places := func() int {
		held.mu.Lock()
		defer held.mu.Unlock()
		return held.all
	}
	for i := 0; places() != 1; i++ {
		if i > 1000 {
			t.Fatal("the request for a starting function was not held")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ready.Store(true)
	<-serving
	if n := places(); n != 0 {
		t.Fatalf("%d places taken while the request is being served", n)
	}
	finished()
	if code := <-done; code != 200 {
		t.Fatalf("status %d", code)
	}
}

// A wake that fails before it is sent (a wake URL that is no URL) has no status of the portal's. The
// caller is told 503 and to retry: a handler that wrote nothing would answer an empty 200, which a
// caller takes for the function's own answer.
func TestWakeErrorWithoutStatusIs503(t *testing.T) {
	none := func(context.Context, Fn) ([]string, error) { return nil, ErrNotFound }
	var lookups atomic.Int32
	s, _ := fnServer(t, fnLookup(80, &lookups), none, NewWaker("http://127.0.0.1:1/\x7f", "tok"))
	req, _ := http.NewRequest("GET", s.URL, nil)
	req.Host = fnHostA + "." + fnDom
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 503 || res.Header.Get("Retry-After") != "2" || !strings.Contains(string(body), "could not be started") {
		t.Fatalf("got %d retry=%q %q", res.StatusCode, res.Header.Get("Retry-After"), body)
	}
}
