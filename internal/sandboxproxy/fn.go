package sandboxproxy

// fn.go: function mode. The same binary, started with FUNCTION_DOMAIN, also routes
// <name>-<6 hex>.<domain> to the instances of a serverless function.
//
//	host label → the function's Deployment (label proxcloud.io/function-host) → a random ready pod
//
// Every other host takes the sandbox path as before (WithFunctions).
//
// A function with no instance is woken through the Waker (POST {URL}/{id}/wake, which only sets
// the scale) and the request is held until a pod is ready. The proxy also tells the portal which
// functions have traffic (Activity), which is what keeps them from the idle reaper, and how much
// (their load), which is what the portal scales a function to more instances on. It only ever
// reads the cluster.
//
// A function with several instances gets each request on one of its ready instances picked at
// random: every instance an equal share, with no limit of its own on requests in flight (the
// function's concurrency is the portal's scaling target, not something the proxy enforces).
//
// This proxy is the edge: TLS ends in it and anyone who reaches its address can send it requests.
// So:
//   - hosts are answered from one list of the function Deployments, read every fnCacheTTL
//     (K8sFnLookup): a name nobody has costs the cluster nothing, however many are asked for, and
//     nothing of the Host header ever reaches a label selector;
//   - the requests held for an instance are bounded, per function and per replica (fnHeld): one
//     over the bound is answered 503 at once;
//   - the client address: nothing the client sent is believed (fnForwarded).

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// What the function package writes on a function's Deployment and pods.
const (
	fnLabelID             = "proxcloud.io/function-id"
	fnLabelHost           = "proxcloud.io/function-host"
	fnAnnotationPort      = "proxcloud.io/function-port"
	fnAnnotationSuspended = "proxcloud.io/function-suspended"
)

const (
	// ponytail: LIST polling with a 2 s cache instead of a watch; replace with an informer when
	// request volume or function count makes LIST load matter.
	fnCacheTTL = 2 * time.Second
	// fnPoll is how often a held request looks for a ready pod, and how long "no ready pod" is
	// remembered.
	fnPoll = 100 * time.Millisecond
	// fnActivityBatch is the most ids the portal takes in one activity report.
	fnActivityBatch = 1000
)

// FnActivityEvery is the period of the activity report. The portal is told it with every report.
const FnActivityEvery = 5 * time.Second

// fnActivityTimeout bounds one report: half a period, so that a report that hangs costs one period
// and no more (a variable so tests need not wait for it).
var fnActivityTimeout = FnActivityEvery / 2

var fnIDRe = regexp.MustCompile(`^fn-[0-9a-f]{16}$`)

// Fn is a function as its Deployment describes it.
type Fn struct {
	ID        string // the Deployment's name
	Namespace string
	Port      int
	Suspended bool
}

// FnLookup resolves the first label of a function host. ErrNotFound when no function has it. It
// is asked on every request, with whatever name a client made up, so it must answer from memory
// (K8sFnLookup).
type FnLookup func(ctx context.Context, host string) (Fn, error)

// FnPods returns the addresses (ip:port) of a function's ready pods: ErrNotFound when it has no
// pod, ErrNotReady when its pods are still starting.
type FnPods func(ctx context.Context, f Fn) ([]string, error)

// FnLister is the slice of kamajik8s.Client function mode uses. Read-only.
type FnLister interface {
	ListCRsAllNamespaces(ctx context.Context, group, version, plural, labelSelector string) ([]map[string]any, error)
	ListPods(ctx context.Context, ns, labelSelector string) ([]map[string]any, error)
}

// K8sFnLookup answers every host label from one LIST of the function Deployments (the ones that
// carry a host label), kept for fnCacheTTL and shared by the requests that find it old. Anyone
// can ask the proxy for any number of names: one nobody has is not in the list and costs the
// cluster nothing. A LIST that fails is kept for fnPoll, so a cluster that does not answer is not
// asked by every request.
func K8sFnLookup(c FnLister) FnLookup {
	all := &fnCache[map[string]Fn]{okTTL: fnCacheTTL, errTTL: fnPoll}
	return func(ctx context.Context, host string) (Fn, error) {
		fns, err := all.get(ctx, "", func(ctx context.Context) (map[string]Fn, error) {
			items, err := c.ListCRsAllNamespaces(ctx, "apps", "v1", "deployments", sandbox.LabelManagedBy+"="+sandbox.ManagedByValue+","+fnLabelHost)
			if err != nil {
				return nil, err
			}
			fns := make(map[string]Fn, len(items))
			for _, it := range items {
				md, _ := it["metadata"].(map[string]any)
				lbl, _ := md["labels"].(map[string]any)
				label, _ := lbl[fnLabelHost].(string)
				if f, ok := fnOf(it); ok && label != "" {
					fns[label] = f
				}
			}
			return fns, nil
		})
		if f, ok := fns[host]; ok {
			return f, nil
		}
		return Fn{}, cmp.Or(err, ErrNotFound)
	}
}

// fnOf reads a function off its Deployment: false for anything that is not the Deployment of a
// function in a function namespace, or is being deleted.
func fnOf(it map[string]any) (Fn, bool) {
	md, _ := it["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	name, _ := md["name"].(string)
	ns, _ := md["namespace"].(string)
	ps, _ := ann[fnAnnotationPort].(string)
	port, err := parsePort(ps)
	if err != nil || !fnIDRe.MatchString(name) || !strings.HasPrefix(ns, "fn-") || md["deletionTimestamp"] != nil {
		return Fn{}, false
	}
	return Fn{ID: name, Namespace: ns, Port: port, Suspended: ann[fnAnnotationSuspended] == "true"}, true
}

// K8sFnPods lists a function's pods and keeps the ones that may get traffic: Running, Ready (the
// port accepts connections) and not being deleted. A pod that is going away counts for nothing:
// a function scaled to zero must be woken, not waited for.
//
// Each pod is reached on the port of its own snapshot (its function-port annotation): while a
// rollout changes the port, the Deployment already names the new one and the pod that is still
// serving listens on the old one. A pod without the annotation gets the Deployment's.
func K8sFnPods(c FnLister) FnPods {
	return func(ctx context.Context, f Fn) ([]string, error) {
		items, err := c.ListPods(ctx, f.Namespace, fnLabelID+"="+f.ID)
		if err != nil {
			return nil, err
		}
		var addrs []string
		starting := false
		for _, it := range items {
			md, _ := it["metadata"].(map[string]any)
			if md["deletionTimestamp"] != nil {
				continue
			}
			st, _ := it["status"].(map[string]any)
			ip, _ := st["podIP"].(string)
			switch phase, _ := st["phase"].(string); {
			case phase == "Running" && ip != "" && podReady(st):
				port := f.Port
				ann, _ := md["annotations"].(map[string]any)
				if ps, _ := ann[fnAnnotationPort].(string); ps != "" {
					if p, err := parsePort(ps); err == nil {
						port = p
					}
				}
				addrs = append(addrs, net.JoinHostPort(ip, strconv.Itoa(port)))
			case phase == "Pending" || phase == "Running":
				starting = true
			}
		}
		switch {
		case len(addrs) > 0:
			return addrs, nil
		case starting:
			return nil, ErrNotReady
		}
		return nil, ErrNotFound
	}
}

type fnEntry[T any] struct {
	v       T
	err     error
	expires time.Time
}

// fnCache keeps the answer of a cluster read per key: results for okTTL, ErrNotFound and
// ErrNotReady for missTTL, other errors for errTTL (0 = not at all). Callers that miss together
// share one read, so the callers held on a cold function cost one LIST per poll, not one each.
// (Cache, the sandbox one, has neither the shared read nor a short life for "not ready".)
type fnCache[T any] struct {
	okTTL, missTTL, errTTL time.Duration
	sf                     singleflight.Group
	mu                     sync.Mutex
	m                      map[string]fnEntry[T]
}

func (c *fnCache[T]) cached(key string) (fnEntry[T], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	return e, ok && time.Now().Before(e.expires)
}

func (c *fnCache[T]) forget(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

func (c *fnCache[T]) get(ctx context.Context, key string, read func(context.Context) (T, error)) (T, error) {
	if e, ok := c.cached(key); ok {
		return e.v, e.err
	}
	v, _, _ := c.sf.Do(key, func() (any, error) {
		if e, ok := c.cached(key); ok { // a read that ended after the check above
			return e, nil
		}
		// The answer is shared, so the caller that happens to run the read must not take it
		// down with its own request. The kube client bounds the call.
		v, err := read(context.WithoutCancel(ctx))
		e := fnEntry[T]{v: v, err: err}
		switch {
		case err == nil:
			e.expires = time.Now().Add(c.okTTL)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotReady):
			e.expires = time.Now().Add(c.missTTL)
		case c.errTTL > 0:
			e.expires = time.Now().Add(c.errTTL)
		default:
			return e, nil
		}
		c.mu.Lock()
		if c.m == nil || len(c.m) > 4096 { // ponytail: wholesale reset bounds memory, as in Cache
			c.m = map[string]fnEntry[T]{}
		}
		c.m[key] = e
		c.mu.Unlock()
		return e, nil
	})
	e := v.(fnEntry[T])
	return e.v, e.err
}

// fnHeld counts the requests held for an instance. The wake call alone is bounded by the Waker;
// without this every request for a function that has no ready pod would be parked for up to the
// Waker's Wait in the proxy that carries all sandbox traffic.
type fnHeld struct {
	mu  sync.Mutex
	per map[string]int
	all int
}

// enter takes a place for a request of function id: false when the function or the replica is at
// its bound.
func (h *fnHeld) enter(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.all >= heldPerReplica || h.per[id] >= heldPerFunction {
		return false
	}
	if h.per == nil {
		h.per = map[string]int{}
	}
	h.per[id]++
	h.all++
	return true
}

func (h *fnHeld) leave(id string) {
	h.mu.Lock()
	if h.per[id]--; h.per[id] <= 0 {
		delete(h.per, id)
	}
	h.all--
	h.mu.Unlock()
}

// fnForwarded sets what an instance is told of the client. TLS ends in this proxy and nothing in
// front of it is trusted, so nothing the client sent is believed: X-Forwarded-For is one address,
// the peer's, X-Forwarded-Proto is what this listener served, and every other X-Forwarded-*
// header (the reverse proxy itself drops only For, Host, Proto and Forwarded), True-Client-IP,
// X-Real-IP and CF-* are dropped, which a framework may believe as readily and which here are
// only ever the client's word.
func fnForwarded(pr *httputil.ProxyRequest) {
	for h := range pr.Out.Header {
		if strings.HasPrefix(h, "X-Forwarded-") || h == "True-Client-Ip" || h == "X-Real-Ip" || strings.HasPrefix(h, "Cf-") {
			delete(pr.Out.Header, h)
		}
	}
	pr.SetXForwarded()
}

// FunctionHandler returns the proxy handler for functions under domain (lowercase). waker
// (required) must point at the portal's function routes: POST {URL}/{id}/wake and POST
// {URL}/activity. lookup is asked on every request, so it must answer from memory (K8sFnLookup).
// A host that is not a function host is answered 404: WithFunctions puts the sandbox handler
// behind it.
func FunctionHandler(domain string, lookup FnLookup, pods FnPods, waker *Waker, act *Activity, log *slog.Logger) http.Handler {
	return functionHandler(domain, lookup, pods, waker, act, &fnHeld{}, log)
}

func functionHandler(domain string, lookup FnLookup, pods FnPods, waker *Waker, act *Activity, held *fnHeld, log *slog.Logger) http.Handler {
	tr := upstreamTransport()
	ready := &fnCache[[]string]{okTTL: fnCacheTTL, missTTL: fnPoll}
	fail := func(w http.ResponseWriter, code int, retryAfter, msg string) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		writeJSON(w, code, map[string]any{"message": msg, "code": code})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// Nothing is looked up, and nothing is kept, for a host that is not a function host.
		host, ok := functionHost(r.Host, domain)
		if !ok {
			fail(w, http.StatusNotFound, "", "The function was not found")
			return
		}
		f, err := lookup(ctx, host)
		if errors.Is(err, ErrNotFound) {
			fail(w, http.StatusNotFound, "", "The function was not found")
			return
		}
		if err != nil {
			log.Error("function lookup failed", "host", host, "err", err)
			fail(w, http.StatusBadGateway, "", "Function lookup failed")
			return
		}
		if f.Suspended {
			fail(w, http.StatusServiceUnavailable, "", "The function is suspended")
			return
		}
		// A held request is activity too: the instance it is waiting for must not be reaped.
		act.begin(f.ID)
		defer act.end(f.ID)

		body := &fnBody{ReadCloser: r.Body}
		defer body.ReadCloser.Close() // what the reverse proxy does on its way out, for the real body
		// sent: the transport had a connection to an instance, so the request may have reached it.
		var sent atomic.Bool
		// The wrapped body goes on a copy of the request. The server looks at the Body of its own
		// request to see whether the client's connection can take another request; with a type it
		// does not know there, it would read what is left of an unread body as the next request.
		r = r.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) { sent.Store(true) },
		}))
		r.Body = body
		holding := false
		release := func() {
			if holding {
				held.leave(f.ID)
				holding = false
			}
		}
		defer release()
		deadline := time.Now().Add(waker.Wait)
		woke := false
		for failed := ""; ; { // twice at most: see the ErrorHandler
			var addrs []string
			for {
				addrs, err = ready.get(ctx, f.ID, func(ctx context.Context) ([]string, error) { return pods(ctx, f) })
				if err == nil {
					break
				}
				if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNotReady) {
					log.Error("function pod lookup failed", "function", f.ID, "err", err)
					fail(w, http.StatusBadGateway, "", "Function lookup failed")
					return
				}
				// From here the request waits for an instance: within the bounds only.
				if !holding {
					if !held.enter(f.ID) {
						fail(w, http.StatusServiceUnavailable, "2", "Too many requests are waiting for the function")
						return
					}
					holding = true
				}
				switch {
				case errors.Is(err, ErrNotFound) && !woke: // no instance: ask for one, once
					werr := waker.Wake(ctx, f.ID, f.Port, "", "")
					var we *WakeError
					switch {
					case werr == nil:
					case errors.Is(werr, ErrNotFound): // the portal does not know it: it is being deleted
						fail(w, http.StatusNotFound, "", "The function was not found")
						return
					case errors.As(werr, &we) && we.Status == http.StatusForbidden: // suspended, or its project is
						fail(w, http.StatusServiceUnavailable, "", "The function is not available")
						return
					case errors.As(werr, &we):
						log.Warn("function wake", "function", f.ID, "err", werr)
						fail(w, http.StatusServiceUnavailable, we.RetryAfter, "The function could not be started yet")
						return
					default:
						return // the client went away
					}
					woke = true
					ready.forget(f.ID)
					continue
				case !time.Now().Before(deadline):
					fail(w, http.StatusServiceUnavailable, "2", "The function is starting")
					return
				}
				select {
				case <-time.After(fnPoll):
				case <-ctx.Done():
					return
				}
			}
			release()
			if failed != "" && len(addrs) > 1 { // not the one that just refused, should the cluster still list it
				addrs = slices.DeleteFunc(slices.Clone(addrs), func(a string) bool { return a == failed })
			}

			target := &url.URL{Scheme: "http", Host: addrs[rand.IntN(len(addrs))]} // an equal share each
			again := false
			(&httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.SetURL(target)
					pr.Out.Host = pr.In.Host // SetURL rewrites Host; keep the original
					fnForwarded(pr)
				},
				Transport:     tr,
				FlushInterval: -1,
				ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
					var oe *net.OpError
					if errors.As(err, &oe) && oe.Op == "dial" {
						ready.forget(f.ID) // the pod is gone: look again
						// A request that never had a connection was not sent and can go to another
						// instance: a rollout or a scale-down stops a pod while its address is still
						// cached here. Once. A dial error alone does not say so: the transport dials
						// again by itself for a request it may replay (a GET, say) after it was
						// written to a kept-alive connection that then failed.
						if again = failed == "" && !sent.Load() && !body.read.Load() && ctx.Err() == nil; again {
							return
						}
						fail(w, http.StatusBadGateway, "", "The function instance is not reachable")
						return
					}
					log.Warn("upstream error", "function", f.ID, "err", err)
					fail(w, http.StatusBadGateway, "", "Bad gateway")
				},
			}).ServeHTTP(w, r)
			if !again {
				return
			}
			failed = target.Host
			body.closed.Store(false)
		}
	})
}

// fnBody is the body of a request that may be offered to a second instance. The transport closes
// a request's body when it gives up on it, also when it never connected; Close here only stops
// further reads, so that a body nobody read is still whole for the second attempt. The handler
// closes the real body when it is done.
type fnBody struct {
	io.ReadCloser
	read, closed atomic.Bool
}

func (b *fnBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, http.ErrBodyReadAfterClose
	}
	b.read.Store(true)
	return b.ReadCloser.Read(p)
}

func (b *fnBody) Close() error {
	b.closed.Store(true)
	return nil
}

// Activity is which functions have traffic: the ones that got a request since the last report
// and the ones with a request in flight (a stream or WebSocket can outlast many reports).
//
// The periodic report alone would leave a request that ends a quiet spell unknown to the portal
// for up to a period, and the reaper would stop the instance under it. So the first request of a
// function that was in neither this period nor the last report is reported at once (first).
//
// The periodic report is sent when it is empty too, under the name of this replica: what the
// portal may take for idle is what the reports it received left out, not what it has not heard
// of. A replica whose reports do not arrive is one that may be serving anything.
//
// The periodic report also carries each function's load in the period: the most requests it had
// in flight at once (a request that outlasts the period counts in the next one too) and how many
// it started. The portal adds up the load of every replica and scales the function to it.
type Activity struct {
	replica  string // this proxy among the replicas: the host name, which in a pod is the pod's name
	mu       sync.Mutex
	inflight map[string]int
	peak     map[string]int  // the most in flight at once this period
	requests map[string]int  // started this period
	seen     map[string]bool // since the last report
	prev     map[string]bool // in the last report: stamped less than a period ago
	first    chan string     // to report now; when full the periodic report carries them
}

// fnLoad is one function's load in a report period.
type fnLoad struct {
	Peak     int `json:"peak"`
	Requests int `json:"requests"`
}

// NewActivity returns an empty Activity.
func NewActivity() *Activity {
	host, _ := os.Hostname()
	return &Activity{replica: host, inflight: map[string]int{}, peak: map[string]int{}, requests: map[string]int{},
		seen: map[string]bool{}, prev: map[string]bool{}, first: make(chan string, fnActivityBatch)}
}

func (a *Activity) begin(id string) {
	a.mu.Lock()
	a.inflight[id]++
	a.requests[id]++
	a.peak[id] = max(a.peak[id], a.inflight[id])
	quiet := !a.seen[id] && !a.prev[id]
	a.seen[id] = true
	a.mu.Unlock()
	if quiet {
		select {
		case a.first <- id:
		default:
		}
	}
}

func (a *Activity) end(id string) {
	a.mu.Lock()
	if a.inflight[id]--; a.inflight[id] <= 0 {
		delete(a.inflight, id)
	}
	a.mu.Unlock()
}

// take returns the functions to report and their load, and starts a new period.
func (a *Activity) take() ([]string, map[string]fnLoad) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.inflight {
		a.seen[id] = true
	}
	ids := slices.Collect(maps.Keys(a.seen))
	a.prev, a.seen = a.seen, map[string]bool{}
	load := make(map[string]fnLoad, len(a.peak))
	for id, p := range a.peak {
		load[id] = fnLoad{Peak: p, Requests: a.requests[id]}
	}
	a.peak, a.requests = maps.Clone(a.inflight), map[string]int{} // what is in flight goes on
	// This report carries whatever was still waiting to be sent at once.
	for pending := true; pending; {
		select {
		case <-a.first:
		default:
			pending = false
		}
	}
	return ids, load
}

// Report sends one activity report through w: every function that has traffic, or none. Functions
// of a report that failed are sent again with the next one: a lost report would let the reaper
// stop a function that is serving.
//
// The request that ends the report is the one the portal takes as "this is all": the ones before
// it, and the last one too when one of them failed, are marked partial. It carries the load, of
// the fnActivityBatch functions with the highest peak; the load of a report that failed is not
// sent again (the next period has its own).
//
// ponytail: the functions of a report in several requests are stamped as each request arrives,
// the report as its last one does; exact while a proxy has at most fnActivityBatch functions with
// traffic, or its requests are answered within seconds.
func (a *Activity) Report(ctx context.Context, w *Waker, log *slog.Logger) {
	a.report(ctx, w, log, true)
}

// Stop sends the last report of a replica that shuts down, once its requests have drained: the
// functions it had traffic for since the report before, and no load. The replicas that took its
// traffic report it from now on; the load of its last period, left on record, would count on top
// of theirs for two periods and scale every loaded function up on each restart of a proxy.
func (a *Activity) Stop(ctx context.Context, w *Waker, log *slog.Logger) {
	a.report(ctx, w, log, false)
}

func (a *Activity) report(ctx context.Context, w *Waker, log *slog.Logger, withLoad bool) {
	all, load := a.take()
	if !withLoad {
		load = map[string]fnLoad{}
	}
	chunks := slices.Collect(slices.Chunk(all, fnActivityBatch))
	if len(chunks) == 0 {
		chunks = [][]string{{}}
	}
	if len(load) > fnActivityBatch { // ponytail: the busiest only, as many as the portal takes
		ids := slices.SortedFunc(maps.Keys(load), func(x, y string) int { return load[y].Peak - load[x].Peak })
		for _, id := range ids[fnActivityBatch:] {
			delete(load, id)
		}
	}
	failed := false
	for i, ids := range chunks {
		last := i == len(chunks)-1
		var l map[string]fnLoad
		if last {
			l = load
		}
		if err := w.reportActivity(ctx, a.replica, ids, failed || !last, l); err != nil {
			log.Warn("function activity report", "functions", len(ids), "err", err)
			failed = true
			a.mu.Lock()
			for _, id := range ids {
				a.seen[id] = true
			}
			a.mu.Unlock()
		}
	}
}

// Run reports every interval, and a function's first request after a quiet spell at once, until
// ctx ends.
func (a *Activity) Run(ctx context.Context, w *Waker, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Report(ctx, w, log)
		case id := <-a.first:
			ids := []string{id}
			for more := true; more && len(ids) < fnActivityBatch; {
				select {
				case id := <-a.first:
					ids = append(ids, id)
				default:
					more = false
				}
			}
			// On failure they are still in seen: the next periodic report carries them.
			if err := w.reportActivity(ctx, a.replica, ids, true, nil); err != nil {
				log.Warn("function activity report", "functions", len(ids), "err", err)
			}
		}
	}
}

// reportActivity: POST {URL}/activity {"ids": [...], "every": seconds, "replica": name, "partial":
// bool, "load": {id: {"peak": n, "requests": n}}}, same bearer as the wake. partial says the ids
// are not all the replica has traffic for; load is absent from a partial report.
func (w *Waker) reportActivity(ctx context.Context, replica string, ids []string, partial bool, load map[string]fnLoad) error {
	ctx, cancel := context.WithTimeout(ctx, fnActivityTimeout)
	defer cancel()
	b := map[string]any{"ids": ids, "every": int(FnActivityEvery / time.Second), "replica": replica, "partial": partial}
	if load != nil {
		b["load"] = load
	}
	body, _ := json.Marshal(b)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL+"/activity", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.Client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("activity report: status %d", resp.StatusCode)
	}
	return nil
}
