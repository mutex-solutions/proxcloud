package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
)

// apiServer is a tenant API server over HTTPS for the controller with its watches: the real client (kamajik8s) talks
// to it. It answers as kube-apiserver does where the controller depends on it:
//
//   - every write gets the next resourceVersion and is an event for the watches of its kind, the controller's own
//     writes too (its finalizer and its status come back to it as MODIFIED);
//   - a watch without a resourceVersion first sends every object as ADDED, one with a resourceVersion sends what
//     happened since, and one older than what the server kept is answered 410 Gone;
//   - a Service that is deleted while it has a finalizer gets a deletionTimestamp and goes when the last finalizer is
//     taken off;
//   - a merge patch of a Service names the resourceVersion it was read at, and is a conflict at another.
//
// Of Secrets it serves a GET of one name and a watch of one name (field selector metadata.name); anything else of a
// Secret fails the test, as would a list of Secrets by an identity that may only get them.
type apiServer struct {
	t  *testing.T
	mu sync.Mutex
	rv int
	// objects by "kind/namespace/name" (a Node has no namespace), and every event since the server started.
	objects map[string]map[string]any
	log     []apiEvent
	kept    int // events up to this resourceVersion are forgotten: 410 Gone
	// streams are the watches that are open, by kind; closing a stream's channel cuts it off.
	streams map[chan struct{}]string
	lists   map[string]int
	watches map[string]int
	client  *kamajik8s.Client
}

type apiEvent struct {
	rv        int
	typ, key  string
	objectDoc []byte
}

func newAPIServer(t *testing.T) *apiServer {
	a := &apiServer{t: t, objects: map[string]map[string]any{}, streams: map[chan struct{}]string{}, lists: map[string]int{}, watches: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/services", func(w http.ResponseWriter, r *http.Request) { a.collection(w, r, "services", "services/") })
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, r *http.Request) { a.collection(w, r, "nodes", "nodes/") })
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/secrets", func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutPrefix(r.URL.Query().Get("fieldSelector"), "metadata.name=")
		if r.URL.Query().Get("watch") != "1" || !ok || name == "" {
			t.Errorf("the controller asked for the Secrets of a namespace: %s", r.URL)
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
			return
		}
		a.collection(w, r, "secrets", "secrets/"+r.PathValue("ns")+"/"+name)
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		obj, ok := a.objects["secrets/"+r.PathValue("ns")+"/"+r.PathValue("name")]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(obj)
	})
	mux.HandleFunc("PATCH /api/v1/namespaces/{ns}/services/{name}", func(w http.ResponseWriter, r *http.Request) {
		var patch struct {
			Metadata struct {
				Finalizers      []any  `json:"finalizers"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&patch)
		a.mu.Lock()
		defer a.mu.Unlock()
		key := "services/" + r.PathValue("ns") + "/" + r.PathValue("name")
		obj, ok := a.objects[key]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		meta := obj["metadata"].(map[string]any)
		if meta["resourceVersion"] != patch.Metadata.ResourceVersion {
			http.Error(w, `{"message":"the object has been modified"}`, http.StatusConflict)
			return
		}
		meta["finalizers"] = patch.Metadata.Finalizers
		if meta["deletionTimestamp"] != nil && len(patch.Metadata.Finalizers) == 0 {
			a.write(key, nil)
		} else {
			a.write(key, obj)
		}
		_ = json.NewEncoder(w).Encode(obj)
	})
	mux.HandleFunc("PATCH /api/v1/namespaces/{ns}/services/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		var patch struct {
			Status map[string]any `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&patch)
		a.mu.Lock()
		defer a.mu.Unlock()
		key := "services/" + r.PathValue("ns") + "/" + r.PathValue("name")
		obj, ok := a.objects[key]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		obj["status"] = patch.Status
		a.write(key, obj)
		_ = json.NewEncoder(w).Encode(obj)
	})
	mux.HandleFunc("POST /api/v1/namespaces/{ns}/events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	})
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	kc, err := kamajik8s.BuildKubeconfig("tenant", ts.URL, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), "admin-token")
	if err != nil {
		t.Fatal(err)
	}
	if a.client, err = kamajik8s.New(kc); err != nil {
		t.Fatal(err)
	}
	return a
}

// write stores obj under key at the next resourceVersion and tells the watches; nil removes it. With a.mu held.
func (a *apiServer) write(key string, obj map[string]any) {
	a.rv++
	typ := "MODIFIED"
	switch _, had := a.objects[key]; {
	case obj == nil:
		typ, obj = "DELETED", a.objects[key]
		delete(a.objects, key)
	case !had:
		typ, a.objects[key] = "ADDED", obj
	default:
		a.objects[key] = obj
	}
	obj["metadata"].(map[string]any)["resourceVersion"] = strconv.Itoa(a.rv)
	doc, _ := json.Marshal(obj)
	a.log = append(a.log, apiEvent{rv: a.rv, typ: typ, key: key, objectDoc: doc})
	for ch := range a.streams {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// set is a write by somebody else: kubectl, a kubelet, cert-manager.
func (a *apiServer) set(key string, obj map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	kind, rest, _ := strings.Cut(key, "/")
	ns, name, _ := strings.Cut(rest, "/")
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["name"] = name
	if kind != "nodes" {
		meta["namespace"] = ns
	}
	if old, ok := a.objects[key]; ok {
		om := old["metadata"].(map[string]any)
		meta["uid"], meta["creationTimestamp"], meta["finalizers"] = om["uid"], om["creationTimestamp"], om["finalizers"]
		if _, set := obj["status"]; !set && kind == "services" {
			obj["status"] = old["status"]
		}
	} else if meta["uid"] == nil {
		meta["uid"], meta["creationTimestamp"] = "uid-"+name, fmt.Sprintf("2026-11-23T10:00:%02dZ", a.rv%60)
	}
	a.write(key, obj)
}

// remove is a delete by somebody else: an object with a finalizer stays, with a deletionTimestamp.
func (a *apiServer) remove(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	obj := a.objects[key]
	meta := obj["metadata"].(map[string]any)
	if fins, _ := meta["finalizers"].([]any); len(fins) > 0 {
		meta["deletionTimestamp"] = "2026-11-23T11:00:00Z"
		a.write(key, obj)
		return
	}
	a.write(key, nil)
}

func (a *apiServer) get(key string) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	doc, _ := json.Marshal(a.objects[key])
	var out map[string]any
	_ = json.Unmarshal(doc, &out)
	return out
}

func (a *apiServer) count(of map[string]int, kind string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return of[kind]
}

func (a *apiServer) open(kind string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, k := range a.streams {
		if k == kind {
			n++
		}
	}
	return n
}

// cutStreams cuts every open watch off in the middle of its answer and forgets every event so far, as an API server
// does that restarts: the watches that come back with the resourceVersion they had are told 410 Gone.
func (a *apiServer) cutStreams() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.streams {
		close(ch)
		delete(a.streams, ch)
	}
	a.kept = a.rv
}

// collection answers a list, or a watch of the objects whose key starts with prefix.
func (a *apiServer) collection(w http.ResponseWriter, r *http.Request, kind, prefix string) {
	q := r.URL.Query()
	a.mu.Lock()
	if q.Get("watch") != "1" {
		a.lists[kind]++
		items := []any{}
		for _, key := range slices.Sorted(maps.Keys(a.objects)) {
			if strings.HasPrefix(key, prefix) {
				items = append(items, a.objects[key])
			}
		}
		doc, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": strconv.Itoa(a.rv)}, "items": items})
		a.mu.Unlock()
		_, _ = w.Write(doc)
		return
	}
	a.watches[kind]++
	from := a.rv
	var first [][]byte
	if rv := q.Get("resourceVersion"); rv != "" {
		if from, _ = strconv.Atoi(rv); from < a.kept {
			a.mu.Unlock()
			w.WriteHeader(http.StatusGone)
			fmt.Fprint(w, `{"kind":"Status","status":"Failure","reason":"Expired","code":410}`)
			return
		}
	} else {
		for _, key := range slices.Sorted(maps.Keys(a.objects)) {
			if strings.HasPrefix(key, prefix) {
				doc, _ := json.Marshal(a.objects[key])
				first = append(first, doc)
			}
		}
	}
	ch := make(chan struct{}, 1)
	a.streams[ch] = kind
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.streams, ch)
		a.mu.Unlock()
	}()
	send := func(typ string, doc []byte) {
		fmt.Fprintf(w, `{"type":%q,"object":%s}`+"\n", typ, doc)
		w.(http.Flusher).Flush()
	}
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	for _, doc := range first {
		send("ADDED", doc)
	}
	for {
		a.mu.Lock()
		var evs []apiEvent
		for _, ev := range a.log {
			if ev.rv > from && strings.HasPrefix(ev.key, prefix) {
				evs = append(evs, ev)
			}
		}
		from = a.rv
		a.mu.Unlock()
		for _, ev := range evs {
			send(ev.typ, ev.objectDoc)
		}
		select {
		case _, ok := <-ch:
			if !ok {
				panic(http.ErrAbortHandler) // cut off: the connection goes without an end of the answer
			}
		case <-r.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
			send("BOOKMARK", []byte(`{"kind":"`+kind+`","metadata":{"resourceVersion":"`+strconv.Itoa(from)+`"}}`))
		}
	}
}

// portal is the gateway's side: what the controller sent and when, answered ready with an address per Service unless
// answer says otherwise.
type portal struct {
	mu     sync.Mutex
	reqs   []*lbgw.Request
	answer func(n int, req *lbgw.Request) *lbgw.Answer
}

func (p *portal) put(_ context.Context, req *lbgw.Request) (*lbgw.Answer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	doc, _ := json.Marshal(req) // a copy: the controller goes on with its request
	var kept lbgw.Request
	_ = json.Unmarshal(doc, &kept)
	p.reqs = append(p.reqs, &kept)
	if p.answer != nil {
		return p.answer(len(p.reqs), req), nil
	}
	ans := &lbgw.Answer{}
	for i, s := range req.Services {
		ans.Services = append(ans.Services, lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: fmt.Sprintf("203.0.113.%d", i+10), IPMode: "VIP"})
	}
	return ans, nil
}

func (p *portal) sent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func (p *portal) last() *lbgw.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) == 0 {
		return &lbgw.Request{}
	}
	return p.reqs[len(p.reqs)-1]
}

// within waits for cond, at most d, and says how long it took.
func within(t *testing.T, d time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < d {
		if cond() {
			return time.Since(start)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("not within %v: %s", d, what)
	return 0
}

// start runs a controller against the API server until the test ends.
func start(t *testing.T, a *apiServer, p *portal, c *controller) {
	t.Helper()
	c.api, c.put, c.log = a.client, p.put, quiet()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func readyNode(name, ip string) map[string]any { return node(name, ip, map[string]any{}) }

// The plan's figure for the controller's part of "a change is live within 15 s": with the times it ships with (a
// turn a second after a change, the unchanged state every 30 s, 10 s while the portal builds), a new Service, a
// changed port, a node that joins and a Service that is deleted are each at the portal within 2 s, and nothing is
// sent when nothing changes, the controller's own writes included.
func TestWatchedChangeIsSentWithinTwoSeconds(t *testing.T) {
	a, p := newAPIServer(t), &portal{}
	a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
	start(t, a, p, &controller{watching: true, interval: 10 * time.Second, resync: resyncEvery, debounce: debounce})
	within(t, 2*time.Second, "the first state", func() bool { return p.sent() >= 1 })

	a.set("services/shop/web", map[string]any{"spec": lbSpec(Class, tcp(80, 30080))})
	within(t, 2*time.Second, "the new Service at the portal", func() bool { return len(p.last().Services) == 1 })
	within(t, 2*time.Second, "its address in its status", func() bool { return len(ingress(a.get("services/shop/web"))) == 1 })
	if got := finalizers(a.get("services/shop/web")); !slices.Contains(got, Finalizer) {
		t.Fatalf("finalizers = %v", got)
	}

	svc := a.get("services/shop/web")
	svc["spec"] = lbSpec(Class, tcp(80, 30080), tcp(443, 30443))
	a.set("services/shop/web", svc)
	within(t, 2*time.Second, "the second port at the portal", func() bool {
		l := p.last().Services
		return len(l) == 1 && len(l[0].Ports) == 2
	})

	a.set("nodes//w2", readyNode("w2", "10.0.0.12"))
	within(t, 2*time.Second, "the second node at the portal", func() bool { return len(p.last().Nodes) == 2 })

	// Quiet now: the controller's own finalizer and status came back to it as events and were turns, and none of
	// them was a request, nor did one of them write again.
	time.Sleep(2500 * time.Millisecond)
	sent, lists := p.sent(), a.count(a.lists, "services")
	time.Sleep(2500 * time.Millisecond)
	if p.sent() != sent || a.count(a.lists, "services") != lists {
		t.Fatalf("in 2.5 quiet seconds the controller sent %d requests and listed %d times", p.sent()-sent, a.count(a.lists, "services")-lists)
	}

	a.remove("services/shop/web")
	within(t, 2*time.Second, "a state without the deleted Service", func() bool { return len(p.last().Services) == 0 })
	within(t, 2*time.Second, "the Service gone", func() bool { return a.get("services/shop/web") == nil })
}

// A change that asks for no other state is a list and no request: the kubelets' heartbeats do not reach the
// portal. The state that stays the same is sent at the resync, and only then.
func TestWatchedHeartbeatsAreNotSent(t *testing.T) {
	a, p := newAPIServer(t), &portal{}
	a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
	a.set("services/shop/web", map[string]any{"spec": lbSpec(Class, tcp(80, 30080))})
	start(t, a, p, &controller{watching: true, interval: time.Hour, resync: 500 * time.Millisecond, debounce: 20 * time.Millisecond})
	within(t, 2*time.Second, "the Service served", func() bool { return len(ingress(a.get("services/shop/web"))) == 1 })
	time.Sleep(100 * time.Millisecond)
	sent, lists := p.sent(), a.count(a.lists, "nodes")
	for i := range 40 { // 1.2 s of heartbeats
		n := readyNode("w1", "10.0.0.11")
		n["status"].(map[string]any)["conditions"].([]any)[1].(map[string]any)["lastHeartbeatTime"] = fmt.Sprint(i)
		a.set("nodes//w1", n)
		time.Sleep(30 * time.Millisecond)
	}
	if got := a.count(a.lists, "nodes") - lists; got < 10 {
		t.Fatalf("40 heartbeats were %d turns: the watch does not ask for turns", got)
	}
	if got := p.sent() - sent; got < 1 || got > 4 {
		t.Fatalf("40 heartbeats in 1.2 s were %d requests, want the 2 or 3 of a resync every 500 ms", got)
	}
}

// A cluster in which nothing happens: its state is sent at the resync, with no event to ask for the turn, so the
// portal hears from a controller that is there and a change no watch told of is put right.
func TestWatchedQuietStateIsSentAtTheResync(t *testing.T) {
	a, p := newAPIServer(t), &portal{}
	a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
	start(t, a, p, &controller{watching: true, interval: time.Hour, resync: 300 * time.Millisecond, debounce: 20 * time.Millisecond})
	within(t, 2*time.Second, "the first state", func() bool { return p.sent() >= 1 })
	time.Sleep(200 * time.Millisecond) // the watches' first events are turns of their own
	sent := p.sent()
	time.Sleep(1500 * time.Millisecond)
	if got := p.sent() - sent; got < 3 || got > 6 {
		t.Fatalf("%d requests in 1.5 quiet seconds with a resync every 300 ms, want 4 or 5", got)
	}
}

// While the portal builds, its answer changes and nothing in the cluster does: the controller asks again after
// -interval, with or without a watch, and stops asking when the Service is ready.
func TestWatchedBuildingIsAskedAgain(t *testing.T) {
	for _, watching := range []bool{true, false} {
		a := newAPIServer(t)
		p := &portal{answer: func(n int, req *lbgw.Request) *lbgw.Answer {
			ans := &lbgw.Answer{}
			for _, s := range req.Services {
				st := lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateBuilding}
				if n >= 4 {
					st = lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: "203.0.113.10", IPMode: "VIP"}
				}
				ans.Services = append(ans.Services, st)
			}
			return ans
		}}
		a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
		a.set("services/shop/web", map[string]any{"spec": lbSpec(Class, tcp(80, 30080))})
		start(t, a, p, &controller{watching: watching, interval: 50 * time.Millisecond, resync: time.Hour, debounce: 20 * time.Millisecond})
		within(t, 2*time.Second, "the address once the portal says ready", func() bool { return len(ingress(a.get("services/shop/web"))) == 1 })
		time.Sleep(300 * time.Millisecond)
		sent := p.sent()
		time.Sleep(300 * time.Millisecond)
		if p.sent() != sent {
			t.Fatalf("watching %v: %d requests for a state that is ready and did not change", watching, p.sent()-sent)
		}
	}
}

// The portal answers a state it did not have with what its balancer served until then: the pass over the appliances
// starts with the request and is done by the next one (project.LBClusterState answers from the row, then converges).
// Here a Service the portal refused is put right. The request that carries the change is still answered "refused";
// the controller sends the changed state once more after -interval, with or without a watch, writes the address
// that answer has, and then leaves the state until the resync.
func TestChangedStateIsAskedAgain(t *testing.T) {
	for _, watching := range []bool{true, false} {
		a := newAPIServer(t)
		row := map[string]lbgw.ServiceStatus{} // the balancer's row: each Service as the last pass left it
		p := &portal{answer: func(_ int, req *lbgw.Request) *lbgw.Answer {
			ans := &lbgw.Answer{}
			for _, s := range req.Services {
				st, ok := row[s.UID]
				if !ok {
					st = lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateBuilding}
				}
				ans.Services = append(ans.Services, st)
				row[s.UID] = lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: "203.0.113.10", IPMode: "VIP"}
				if len(s.Ports) > 1 {
					row[s.UID] = lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateRefused, Reason: lbgw.ReasonInvalid, Message: "one port only"}
				}
			}
			return ans
		}}
		a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
		a.set("services/shop/web", map[string]any{"spec": lbSpec(Class, tcp(80, 30080), tcp(443, 30443))})
		start(t, a, p, &controller{watching: watching, interval: 50 * time.Millisecond, resync: time.Hour, debounce: 20 * time.Millisecond})
		within(t, 2*time.Second, "the refusal heard", func() bool { return p.sent() >= 2 })
		time.Sleep(200 * time.Millisecond)
		if got := ingress(a.get("services/shop/web")); len(got) != 0 {
			t.Fatalf("watching %v: a refused Service has an address: %v", watching, got)
		}
		svc := a.get("services/shop/web")
		svc["spec"] = lbSpec(Class, tcp(80, 30080))
		a.set("services/shop/web", svc)
		within(t, 2*time.Second, "the address of the Service that was put right", func() bool { return len(ingress(a.get("services/shop/web"))) == 1 })
		time.Sleep(300 * time.Millisecond)
		sent := p.sent()
		time.Sleep(300 * time.Millisecond)
		if p.sent() != sent {
			t.Fatalf("watching %v: %d requests for a state that was asked again and did not change", watching, p.sent()-sent)
		}
	}
}

// A Secret a Service names is watched, by its name, and a Secret that changes is at the portal at once; the
// Secrets nobody names are not watched, and the watch ends when the Service stops naming the Secret.
func TestWatchedSecret(t *testing.T) {
	a, p := newAPIServer(t), &portal{}
	secret := func(crt string) map[string]any {
		return map[string]any{"type": lbgw.TLSSecretType, "data": map[string]any{
			"tls.crt": base64.StdEncoding.EncodeToString([]byte(crt)), "tls.key": base64.StdEncoding.EncodeToString([]byte("KEY"))}}
	}
	hash := func() string {
		l := p.last().Services
		if len(l) != 1 || len(l[0].Certificates) != 1 {
			return ""
		}
		return l[0].Certificates[0].SHA256
	}
	a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
	a.set("secrets/shop/web-tls", secret("CERT 1"))
	a.set("secrets/shop/other", secret("ANOTHER"))
	a.set("services/shop/web", map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{lbgw.OwnPrefix + lbgw.KnobTLSSecret: "web-tls"}},
		"spec":     lbSpec(Class, tcp(443, 30443))})
	start(t, a, p, &controller{watching: true, interval: time.Hour, resync: time.Hour, debounce: 20 * time.Millisecond})
	within(t, 2*time.Second, "the Secret's hash at the portal", func() bool { return hash() == lbgw.SecretHash([]byte("CERT 1"), []byte("KEY")) })
	within(t, 2*time.Second, "the watch of the Secret", func() bool { return a.open("secrets") == 1 })

	a.set("secrets/shop/web-tls", secret("CERT 2"))
	within(t, time.Second, "the renewed Secret's hash at the portal", func() bool { return hash() == lbgw.SecretHash([]byte("CERT 2"), []byte("KEY")) })

	// A Secret no Service names: no watch saw it, so no turn and no request.
	time.Sleep(100 * time.Millisecond)
	sent, lists := p.sent(), a.count(a.lists, "services")
	a.set("secrets/shop/other", secret("ANOTHER 2"))
	time.Sleep(300 * time.Millisecond)
	if p.sent() != sent || a.count(a.lists, "services") != lists {
		t.Fatal("a Secret no Service names was a turn")
	}

	svc := a.get("services/shop/web")
	delete(svc["metadata"].(map[string]any), "annotations")
	a.set("services/shop/web", svc)
	within(t, 2*time.Second, "the end of the Secret's watch", func() bool { return a.open("secrets") == 0 })
	if n := a.count(a.watches, "secrets"); n != 1 {
		t.Fatalf("%d watches of Secrets, want the one of web-tls", n)
	}
}

// An API server that restarts: every stream is cut off and what the watches knew is gone (410). What changed
// while no watch was up is at the portal as soon as the streams end, since their end is a turn, and what changes
// after is seen by the streams that came back.
func TestWatchedStreamsThatEnd(t *testing.T) {
	a, p := newAPIServer(t), &portal{}
	a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
	a.set("services/shop/web", map[string]any{"spec": lbSpec(Class, tcp(80, 30080))})
	start(t, a, p, &controller{watching: true, interval: time.Hour, resync: time.Hour, debounce: 20 * time.Millisecond})
	within(t, 2*time.Second, "the Service served", func() bool { return len(ingress(a.get("services/shop/web"))) == 1 })
	within(t, 2*time.Second, "both watches", func() bool { return a.open("services") == 1 && a.open("nodes") == 1 })

	a.set("nodes//w2", readyNode("w2", "10.0.0.12"))
	a.cutStreams()
	within(t, 2*time.Second, "the node that joined at the cut", func() bool { return len(p.last().Nodes) == 2 })
	within(t, 3*time.Second, "both watches again", func() bool { return a.open("services") == 1 && a.open("nodes") == 1 })

	a.set("nodes//w3", readyNode("w3", "10.0.0.13"))
	within(t, 2*time.Second, "the node that joined after", func() bool { return len(p.last().Nodes) == 3 })
}

// Without -watch, which is the controller under charts 0.1.17 and 0.1.18: no watch is asked of the API server, a
// change is at the portal a turn later, and a state that did not change is not sent at every turn.
func TestPollsWithoutWatch(t *testing.T) {
	a, p := newAPIServer(t), &portal{}
	a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
	start(t, a, p, &controller{interval: 30 * time.Millisecond, resync: time.Hour})
	within(t, 2*time.Second, "the first state", func() bool { return p.sent() >= 1 })
	a.set("services/shop/web", map[string]any{"spec": lbSpec(Class, tcp(80, 30080))})
	within(t, 2*time.Second, "the address", func() bool { return len(ingress(a.get("services/shop/web"))) == 1 })
	time.Sleep(300 * time.Millisecond) // the changed state is sent once more, one turn later
	sent, lists := p.sent(), a.count(a.lists, "services")
	time.Sleep(300 * time.Millisecond)
	if got := a.count(a.lists, "services") - lists; got < 5 {
		t.Fatalf("%d turns in 300 ms at 30 ms each", got)
	}
	if p.sent() != sent {
		t.Fatalf("%d requests for a state that did not change", p.sent()-sent)
	}
	if n := a.count(a.watches, "services") + a.count(a.watches, "nodes") + a.count(a.watches, "secrets"); n != 0 {
		t.Fatalf("%d watches without -watch", n)
	}
}
