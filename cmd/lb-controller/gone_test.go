package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
)

const keepAnnotation = lbgw.OwnPrefix + lbgw.KnobKeepAddress

// heardPortal is the portal's side behind the gateway's own handler: every call as the handler passed it on, in
// order. What the portal makes of a Service that is being deleted is the project side's
// (internal/platform/project, lbClusterKeepGone), shown on PostgreSQL in its integration test with the request this
// test sees the controller send.
type heardPortal struct {
	mu    sync.Mutex
	calls []heardCall
}

type heardCall struct {
	services []lbgw.Service
	gone     []lbgw.Gone
}

func (p *heardPortal) LBClusterState(_ context.Context, _ *lbgw.Credential, services []lbgw.Service, _ []lbgw.Node, _ []lbgw.CertificateData, gone []lbgw.Gone) (*lbgw.Answer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, heardCall{services, gone})
	ans := &lbgw.Answer{}
	for i, s := range services {
		ans.Services = append(ans.Services, lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: fmt.Sprintf("203.0.113.%d", i+10)})
	}
	return ans, nil
}

func (p *heardPortal) heard() []heardCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// kubectl annotate ... proxcloud.io/lb-keep-address=true and kubectl delete, one right after the other (drill R2a-2
// of v0.11.1-rc.2: 0.31 and 0.36 s apart, and the address was given back both times). The API server sends what a
// real one sends, in its order: MODIFIED with the annotation, then MODIFIED with the deletionTimestamp, since the
// controller's finalizer holds the object. Both fall into one debounce, so the controller's turn sees the Service
// only as one that is being deleted and never sends it with the annotation: the portal's last stored state knows
// nothing of the wish. The controller says it itself, from the object, in the request that drops the Service, and
// lets the Service go after that request and not before. The gateway's own handler passes it on.
//
// The other order too: the annotation is removed and the Service deleted at once. The last stored state says "keep",
// the object says nothing of it, and the object's word is the one sent.
func TestServiceAnnotatedJustBeforeItsDeleteSaysSo(t *testing.T) {
	a, p := newAPIServer(t), &heardPortal{}
	a.set("nodes//w1", readyNode("w1", "10.0.0.11"))
	ts := httptest.NewTLSServer(&lbgw.Handler{Reg: oneCluster{}, Portal: p, Log: quiet()})
	defer ts.Close()
	g := &gateway{url: ts.URL, dir: gatewayDir(t, ts)}
	c := &controller{api: a.client, put: g.put, log: quiet(), watching: true, interval: 10 * time.Second, resync: resyncEvery, debounce: debounce}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.run(ctx) }()
	defer func() { cancel(); <-done }()

	a.set("services/shop/web", map[string]any{"spec": lbSpec(Class, tcp(80, 30080))})
	a.set("services/shop/api", map[string]any{"metadata": map[string]any{"annotations": map[string]any{keepAnnotation: "true"}}, "spec": lbSpec(Class, tcp(81, 30081))})
	within(t, 3*time.Second, "both Services served", func() bool {
		return len(ingress(a.get("services/shop/web"))) == 1 && len(ingress(a.get("services/shop/api"))) == 1
	})
	time.Sleep(debounce + 500*time.Millisecond) // the controller's own writes came back and were turns: quiet now
	before := len(p.heard())

	// Both within the debounce, and nothing in between.
	web := a.get("services/shop/web")
	web["metadata"].(map[string]any)["annotations"] = map[string]any{keepAnnotation: "true"}
	a.set("services/shop/web", web)
	a.remove("services/shop/web")
	api := a.get("services/shop/api")
	delete(api["metadata"].(map[string]any), "annotations")
	a.set("services/shop/api", api)
	a.remove("services/shop/api")
	if a.get("services/shop/web") == nil || a.get("services/shop/api") == nil {
		t.Fatal("a Service went before the controller's turn: its finalizer did not hold it")
	}
	within(t, 3*time.Second, "both Services let go", func() bool {
		return a.get("services/shop/web") == nil && a.get("services/shop/api") == nil
	})

	calls := p.heard()
	said := map[string]lbgw.Gone{}
	for i, call := range calls {
		for _, s := range call.services {
			// The condition of the drill: what the portal stored of the Services is from before the last change.
			if _, keeps := s.Annotations[keepAnnotation]; (s.UID == "uid-web" && keeps) || (s.UID == "uid-api" && !keeps) {
				t.Fatalf("request %d has %s as it read after its last change: the two events were not one turn", i, s.UID)
			}
			if i >= before && (s.UID == "uid-web" || s.UID == "uid-api") {
				t.Fatalf("request %d still lists %s, which is being deleted", i, s.UID)
			}
		}
		for _, g := range call.gone {
			if i < before {
				t.Fatalf("request %d names %s as being deleted before it was", i, g.UID)
			}
			said[g.UID] = g
		}
	}
	if len(calls) <= before {
		t.Fatal("no request after the deletes")
	}
	if first := calls[before]; len(first.gone) != 2 {
		t.Fatalf("the request that drops the Services names %+v as being deleted, want both", first.gone)
	}
	if got := said["uid-web"]; got.Annotations[keepAnnotation] != "true" || len(got.Annotations) != 1 {
		t.Fatalf("web was annotated to keep its address and deleted: the portal heard %+v", got)
	}
	if got, ok := said["uid-api"]; !ok || len(got.Annotations) != 0 {
		t.Fatalf("api's annotation was removed and it was deleted: the portal heard %+v (%v)", got, ok)
	}
}

// A portal of before v0.11.1-rc.3 speaks protocol 2 and knows no Services that are being deleted: it decodes
// strictly and refuses the field (a portal rolled back to rc.2). The controller sends it the request without, in
// the same turn, and lets the Service go: that portal goes by its stored state, as it did. A request the portal
// refuses for something else is still a refusal.
func TestGatewayPutWithoutGoneForAPortalThatKnowsNone(t *testing.T) {
	var bodies []string
	bad := false
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(lbgw.ProtocolHeader, "2")
		var req struct { // the request of v0.11.1-rc.2 (internal/cloud/lbgw/gateway.go at da92b501), decoded as there
			Services        []lbgw.Service
			Nodes           []lbgw.Node
			CertificateData json.RawMessage
		}
		raw := new(json.RawMessage)
		_ = json.NewDecoder(r.Body).Decode(raw)
		bodies = append(bodies, string(*raw))
		dec := json.NewDecoder(bytes.NewReader(*raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || bad {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": lbgw.ReasonInvalid, "message": "lb gateway: the body is not a state"})
			return
		}
		_ = json.NewEncoder(w).Encode(lbgw.Answer{Services: []lbgw.ServiceStatus{}})
	}))
	defer ts.Close()
	f := &fakeTenant{services: map[string]map[string]any{}}
	f.add("shop", "web", "u-1", "2026-11-23T10:00:00Z", lbSpec(Class, tcp(80, 30080)))
	meta := f.svc("shop", "web")["metadata"].(map[string]any)
	meta["finalizers"], meta["deletionTimestamp"] = []any{Finalizer}, "2026-11-23T11:00:00Z"
	meta["annotations"] = map[string]any{keepAnnotation: "true"}
	g := &gateway{url: ts.URL, dir: gatewayDir(t, ts)}
	c := &controller{api: f, put: g.put, log: quiet()}
	if err := c.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"gone"`) || strings.Contains(bodies[1], `"gone"`) {
		t.Fatalf("requests %q: want the whole one, then the one without the Services being deleted", bodies)
	}
	if f.svc("shop", "web") != nil {
		t.Fatal("the Service is still held after the portal took the state without it")
	}

	f.add("shop", "web", "u-2", "2026-11-23T10:00:00Z", lbSpec(Class, tcp(80, 30080)))
	meta = f.svc("shop", "web")["metadata"].(map[string]any)
	meta["finalizers"], meta["deletionTimestamp"] = []any{Finalizer}, "2026-11-23T11:00:00Z"
	bad, bodies = true, nil
	if err := c.turn(context.Background()); err == nil || len(bodies) != 2 {
		t.Fatalf("a portal that refuses the state: err %v after %d requests", err, len(bodies))
	}
	if f.svc("shop", "web") == nil {
		t.Fatal("the Service was let go though the portal took no state without it")
	}
}

// A portal of before v0.11.1-rc.3 measures the whole body against the state's 64 KiB before it looks at a field
// (v0.11.0: one MaxBytesReader, internal/cloud/lbgw/gateway.go:330 at the tag; rc.1 and rc.2: the body less its
// certificates). With a state near that limit the Services being deleted put the request over it, and that portal
// answers 413, not the 400 of a field it does not know. The controller sends it the request without them all the
// same, in the same turn, and lets the Service go.
//
// The controls: a 413 to a request that names no Service being deleted is one request, and is said TooLarge on the
// Services; a state that is too large by itself is still refused after the second request, and the Service is held.
func TestGatewayPutWithoutGoneAfter413(t *testing.T) {
	var bodies []string
	full := false // the state alone is over the limit
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(lbgw.ProtocolHeader, "2")
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if full || strings.Contains(string(raw), `"gone"`) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": lbgw.ReasonTooLarge, "message": "The cluster's load balancer state is over 64 KiB and was not taken"})
			return
		}
		var req lbgw.Request
		_ = json.Unmarshal(raw, &req)
		ans := lbgw.Answer{Services: []lbgw.ServiceStatus{}}
		for _, s := range req.Services {
			ans.Services = append(ans.Services, lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: "203.0.113.5"})
		}
		_ = json.NewEncoder(w).Encode(ans)
	}))
	defer ts.Close()
	f := &fakeTenant{services: map[string]map[string]any{}}
	f.add("shop", "api", "u-api", "2026-11-23T10:00:00Z", lbSpec(Class, tcp(81, 30081)))
	f.svc("shop", "api")["metadata"].(map[string]any)["finalizers"] = []any{Finalizer}
	deleted := func(uid string) {
		f.add("shop", "web", uid, "2026-11-23T10:00:00Z", lbSpec(Class, tcp(80, 30080)))
		meta := f.svc("shop", "web")["metadata"].(map[string]any)
		meta["finalizers"], meta["deletionTimestamp"] = []any{Finalizer}, "2026-11-23T11:00:00Z"
		meta["annotations"] = map[string]any{keepAnnotation: "true"}
	}
	g := &gateway{url: ts.URL, dir: gatewayDir(t, ts)}
	c := &controller{api: f, put: g.put, log: quiet()}
	turn := func() ([]string, error) {
		bodies, f.events = nil, nil
		err := c.turn(context.Background())
		return reasons(f.events), err
	}

	deleted("u-1")
	if got, err := turn(); err != nil || slices.Contains(got, "api:TooLarge") {
		t.Fatalf("a 413 to the request with the Services being deleted: %v, Events %v", err, got)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"gone"`) || strings.Contains(bodies[1], `"gone"`) || !strings.Contains(bodies[1], `"u-api"`) {
		t.Fatalf("requests %q: want the whole one, then the same state without the Services being deleted", bodies)
	}
	if f.svc("shop", "web") != nil {
		t.Fatal("the Service is still held after the portal took the state without it")
	}
	if !hasIngress(ingress(f.svc("shop", "api")), "203.0.113.5") {
		t.Fatal("the other Service was not served in that turn")
	}

	// Control: nothing is being deleted and the portal answers 413. One request, and the Services are told.
	full = true
	var rf *lbgw.Refusal
	if got, err := turn(); !errors.As(err, &rf) || rf.Status != http.StatusRequestEntityTooLarge || len(bodies) != 1 || !slices.Equal(got, []string{"api:TooLarge"}) {
		t.Fatalf("a 413 to a request with nothing to leave out: %v after %d requests, Events %v", err, len(bodies), got)
	}
	// Control: the state is too large without the Service being deleted too. Two requests, and the Service is held.
	deleted("u-2")
	if _, err := turn(); !errors.As(err, &rf) || rf.Status != http.StatusRequestEntityTooLarge || len(bodies) != 2 || strings.Contains(bodies[1], `"gone"`) {
		t.Fatalf("a state that is too large by itself: %v after %d requests", err, len(bodies))
	}
	if f.svc("shop", "web") == nil {
		t.Fatal("the Service was let go though the portal took no state without it")
	}
}

// The same limit one protocol down. A portal rolled back to v0.11.0 reads the body under one limit of 64 KiB and
// only then decodes it, strictly (internal/cloud/lbgw/gateway.go:330 at the tag: the handler below does the same).
// A state that the annotations of protocol 2 put over the limit is answered 413 with that portal's protocol, where a
// smaller one is answered 400 for the field. The request of protocol 1 has no annotations and fits.
//
// The controls: a request with nothing of protocol 2 is sent once, and one that is too large as the request of
// protocol 1 too is refused in that form and not sent a third time.
func TestGatewayPutStepsDownAfter413(t *testing.T) {
	type oldService struct { // v0.11.0's Service
		UID, Namespace, Name  string
		Ports                 []lbgw.Port
		ExternalTrafficPolicy string
		HealthCheckNodePort   int
		SourceRanges          []string
		ProxyProtocol         bool
		IPFamilies            []string
	}
	var (
		statuses []int
		got      []oldService
	)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(lbgw.ProtocolHeader, "1")
		answer := func(status int, v any) {
			statuses = append(statuses, status)
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		var req struct {
			Services []oldService
			Nodes    []lbgw.Node
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				answer(http.StatusRequestEntityTooLarge, map[string]string{"reason": lbgw.ReasonTooLarge, "message": "lb gateway: the state takes at most 64 KiB"})
				return
			}
			answer(http.StatusBadRequest, map[string]string{"reason": lbgw.ReasonInvalid, "message": "lb gateway: the body is not a state: " + err.Error()})
			return
		}
		got = req.Services
		answer(http.StatusOK, lbgw.Answer{Services: []lbgw.ServiceStatus{{UID: req.Services[0].UID, State: lbgw.StateReady, Address: "203.0.113.5"}}})
	}))
	defer ts.Close()
	g := &gateway{url: ts.URL, dir: gatewayDir(t, ts)}
	put := func(req *lbgw.Request) (*lbgw.Answer, error) {
		statuses, got = nil, nil
		return g.put(context.Background(), req)
	}
	svc := lbgw.Service{UID: "u-1", Ports: []lbgw.Port{{Protocol: "TCP", Port: 80, NodePort: 30080}}, Annotations: map[string]string{}}
	for i := range 300 {
		svc.Annotations[lbgw.OwnPrefix+"y"+strconv.Itoa(i)] = strings.Repeat("y", lbgw.MaxAnnotationValue)
	}
	req := &lbgw.Request{Services: []lbgw.Service{svc}}

	// The condition of the test: a small request with a field of protocol 2 is the 400 the step-down knew.
	small := &lbgw.Request{Services: []lbgw.Service{{UID: "u-1", Ports: svc.Ports, Annotations: map[string]string{lbgw.OwnPrefix + "y": "y"}}}}
	if _, err := put(small); err != nil || !slices.Equal(statuses, []int{http.StatusBadRequest, http.StatusOK}) {
		t.Fatalf("a small request of protocol 2: %v, answers %v", err, statuses)
	}
	ans, err := put(req)
	if err != nil || !slices.Equal(statuses, []int{http.StatusRequestEntityTooLarge, http.StatusOK}) || len(ans.Services) != 1 || len(got) != 1 || got[0].Ports[0].NodePort != 30080 {
		t.Fatalf("a state over the old portal's limit by its annotations: %+v %v, answers %v, the portal got %+v", ans, err, statuses, got)
	}
	// With a Service being deleted: without it first, which is still too large, then the request of protocol 1.
	req.Gone = []lbgw.Gone{{UID: "u-2"}}
	if _, err := put(req); err != nil || !slices.Equal(statuses, []int{http.StatusRequestEntityTooLarge, http.StatusRequestEntityTooLarge, http.StatusOK}) {
		t.Fatalf("the same with a Service being deleted: %v, answers %v", err, statuses)
	}
	req.Gone = nil

	// Control: nothing of protocol 2 in it. One request, and its 413 is the answer.
	plain := &lbgw.Request{Services: []lbgw.Service{{UID: "u-1", Ports: svc.Ports}}}
	for i := range 6000 {
		plain.Services[0].SourceRanges = append(plain.Services[0].SourceRanges, "203.0."+strconv.Itoa(i%256)+".0/24")
	}
	var rf *lbgw.Refusal
	if _, err := put(plain); !errors.As(err, &rf) || rf.Status != http.StatusRequestEntityTooLarge || rf.Reason != lbgw.ReasonTooLarge || len(statuses) != 1 {
		t.Fatalf("a request with nothing of protocol 2: %v, answers %v", err, statuses)
	}
	// Control: too large as the request of protocol 1 too. Two requests, no third.
	req.Services[0].SourceRanges = plain.Services[0].SourceRanges
	if _, err := put(req); !errors.As(err, &rf) || rf.Status != http.StatusRequestEntityTooLarge || len(statuses) != 2 {
		t.Fatalf("too large in both forms: %v, answers %v", err, statuses)
	}
}

// One request names at most lbgw.MaxGone Services that are being deleted, so a namespace deleted with more of them
// is no request the gateway refuses: the rest are named in the next turn's, and none is let go unnamed.
func TestMoreServicesDeletedThanOneRequestNames(t *testing.T) {
	f := &fakeTenant{services: map[string]map[string]any{}}
	for i := range lbgw.MaxGone + 3 {
		name := fmt.Sprintf("s%02d", i)
		f.add("shop", name, "u-"+name, "2026-11-23T10:00:00Z", lbSpec(Class, tcp(80, 30080)))
		meta := f.svc("shop", name)["metadata"].(map[string]any)
		meta["finalizers"], meta["deletionTimestamp"] = []any{Finalizer}, "2026-11-23T11:00:00Z"
	}
	var named []int
	c := &controller{api: f, log: quiet(), put: func(_ context.Context, req *lbgw.Request) (*lbgw.Answer, error) {
		named = append(named, len(req.Gone))
		return &lbgw.Answer{}, nil
	}}
	if err := c.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.services) != 3 || !c.open {
		t.Fatalf("after the first turn %d Services are held (open %v), want the 3 it did not name", len(f.services), c.open)
	}
	if err := c.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(named, []int{lbgw.MaxGone, 3}) || len(f.services) != 0 {
		t.Fatalf("named %v, %d Services left", named, len(f.services))
	}
}
