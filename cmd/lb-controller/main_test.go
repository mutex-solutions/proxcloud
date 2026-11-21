package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
)

// fakeTenant is a tenant API in memory: Services by namespace/name, Nodes, and what the controller wrote.
type fakeTenant struct {
	services map[string]map[string]any
	nodes    []map[string]any
	events   []map[string]any
	rv       int
}

func (f *fakeTenant) ListServicesAllNamespaces(context.Context) ([]map[string]any, error) {
	var out []map[string]any
	for _, s := range f.services {
		b, _ := json.Marshal(s) // a copy, as an API answer is
		var c map[string]any
		_ = json.Unmarshal(b, &c)
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeTenant) ListNodes(context.Context) ([]map[string]any, error) { return f.nodes, nil }

func (f *fakeTenant) PatchService(_ context.Context, ns, name string, patch map[string]any) error {
	s := f.services[ns+"/"+name]
	meta := s["metadata"].(map[string]any)
	pm := patch["metadata"].(map[string]any)
	if pm["resourceVersion"] != meta["resourceVersion"] {
		return &lbgw.Refusal{Status: http.StatusConflict, Message: "the object has been modified"}
	}
	var fins []any
	for _, f := range pm["finalizers"].([]string) {
		fins = append(fins, f)
	}
	meta["finalizers"] = fins
	f.rv++
	meta["resourceVersion"] = strconv.Itoa(f.rv)
	// The API server removes a deleted object once its last finalizer is gone.
	if meta["deletionTimestamp"] != nil && len(fins) == 0 {
		delete(f.services, ns+"/"+name)
	}
	return nil
}

func (f *fakeTenant) PatchServiceStatus(_ context.Context, ns, name string, patch map[string]any) error {
	s := f.services[ns+"/"+name]
	lb := patch["status"].(map[string]any)["loadBalancer"].(map[string]any)
	ingress := []any{}
	if l, ok := lb["ingress"].([]any); ok {
		ingress = l
	}
	s["status"] = map[string]any{"loadBalancer": map[string]any{"ingress": ingress}}
	return nil
}

func (f *fakeTenant) CreateCore(_ context.Context, plural string, obj map[string]any) error {
	if plural != "events" {
		panic(plural)
	}
	f.events = append(f.events, obj)
	return nil
}

func (f *fakeTenant) add(ns, name, uid, created string, spec map[string]any) {
	f.rv++
	f.services[ns+"/"+name] = map[string]any{
		"metadata": map[string]any{"namespace": ns, "name": name, "uid": uid, "resourceVersion": strconv.Itoa(f.rv), "creationTimestamp": created},
		"spec":     spec,
	}
}

func (f *fakeTenant) svc(ns, name string) map[string]any { return f.services[ns+"/"+name] }

func finalizers(s map[string]any) []string {
	var out []string
	l, _ := s["metadata"].(map[string]any)["finalizers"].([]any)
	for _, f := range l {
		out = append(out, f.(string))
	}
	return out
}

func ingress(s map[string]any) []any {
	st, _ := s["status"].(map[string]any)
	lb, _ := st["loadBalancer"].(map[string]any)
	l, _ := lb["ingress"].([]any)
	return l
}

func lbSpec(class string, ports ...map[string]any) map[string]any {
	spec := map[string]any{"type": "LoadBalancer", "externalTrafficPolicy": "Cluster", "ipFamilies": []any{"IPv4"}}
	if class != "" {
		spec["loadBalancerClass"] = class
	}
	var l []any
	for _, p := range ports {
		l = append(l, p)
	}
	spec["ports"] = l
	return spec
}

func tcp(port, nodePort int) map[string]any {
	return map[string]any{"name": "p" + strconv.Itoa(port), "protocol": "TCP", "port": float64(port), "nodePort": float64(nodePort)}
}

func node(name, ip string, labels map[string]any) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": name, "labels": labels},
		"status":   map[string]any{"addresses": []any{map[string]any{"type": "Hostname", "address": name}, map[string]any{"type": "InternalIP", "address": ip}}},
	}
}

// gatewayFake answers each Service as the test says and records the last request.
type gatewayFake struct {
	last    *lbgw.Request
	answer  func(s lbgw.Service) lbgw.ServiceStatus
	refusal error
}

func (g *gatewayFake) put(_ context.Context, req *lbgw.Request) (*lbgw.Answer, error) {
	g.last = req
	if g.refusal != nil {
		return nil, g.refusal
	}
	ans := &lbgw.Answer{}
	for _, s := range req.Services {
		ans.Services = append(ans.Services, g.answer(s))
	}
	return ans, nil
}

func uids(req *lbgw.Request) []string {
	var out []string
	for _, s := range req.Services {
		out = append(out, s.UID)
	}
	return out
}

func reasons(evs []map[string]any) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e["involvedObject"].(map[string]any)["name"].(string)+":"+e["reason"].(string))
	}
	return out
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// One reconcile story: which Services are owned in each mode, the finalizer before the first PUT, the address in the
// status with its ipMode, one Event per change, the node exclusion label, and letting go of a Service that is deleted,
// changes type or is not of the class.
func TestTurn(t *testing.T) {
	ctx := context.Background()
	f := &fakeTenant{services: map[string]map[string]any{}}
	f.add("web", "classed", "u-1", "2026-11-21T10:00:00Z", lbSpec(Class, tcp(80, 30080), tcp(443, 30443)))
	f.add("web", "plain", "u-2", "2026-11-21T09:00:00Z", lbSpec("", tcp(80, 30081)))
	f.add("web", "internal", "u-3", "2026-11-21T08:00:00Z", lbSpec("proxcloud.io/internal", tcp(80, 30082)))
	f.add("web", "cluster-ip", "u-4", "2026-11-21T08:00:00Z", map[string]any{"type": "ClusterIP", "ports": []any{tcp(80, 0)}})
	f.add("db", "udp", "u-5", "2026-11-21T11:00:00Z", lbSpec(Class, map[string]any{"protocol": "UDP", "port": float64(53), "nodePort": float64(30053)}))
	proxy := lbSpec(Class, tcp(80, 30090))
	proxy["loadBalancerIP"] = "203.0.113.99"
	f.add("web", "proxy", "u-6", "2026-11-21T12:00:00Z", proxy)
	f.svc("web", "proxy")["metadata"].(map[string]any)["annotations"] = map[string]any{proxyAnnotation: "true"}
	f.nodes = []map[string]any{
		node("w1", "10.0.0.11", nil),
		node("w2", "10.0.0.12", map[string]any{excludeLabel: ""}),
		node("w3", "10.0.0.13", nil),
	}
	g := &gatewayFake{answer: func(s lbgw.Service) lbgw.ServiceStatus {
		switch s.UID {
		case "u-5":
			return lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateRefused, Reason: lbgw.ReasonUDPNotSupported, Message: "TCP only"}
		case "u-6":
			return lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: "203.0.113.20"}
		}
		return lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateBuilding}
	}}

	// Class-only mode (an existing cluster): only class proxcloud.io/lb, the oldest first.
	c := &controller{api: f, put: g.put, log: quiet()}
	if err := c.turn(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := uids(g.last), []string{"u-1", "u-5", "u-6"}; !slices.Equal(got, want) {
		t.Fatalf("class-only sent %v, want %v", got, want)
	}
	if got := g.last.Nodes; len(got) != 2 || got[0].Address != "10.0.0.11" || got[1].Address != "10.0.0.13" {
		t.Fatalf("nodes = %+v (w2 carries the exclusion label)", got)
	}
	if s := g.last.Services[0]; len(s.Ports) != 2 || s.Ports[1].NodePort != 30443 || s.Namespace != "web" || s.Name != "classed" {
		t.Fatalf("wire = %+v", s)
	}
	if !g.last.Services[2].ProxyProtocol {
		t.Fatal("the PROXY annotation is not sent")
	}
	for _, n := range []string{"classed", "udp", "proxy"} {
		ns := "web"
		if n == "udp" {
			ns = "db"
		}
		if !slices.Equal(finalizers(f.svc(ns, n)), []string{Finalizer}) {
			t.Fatalf("%s: finalizers = %v", n, finalizers(f.svc(ns, n)))
		}
	}
	if fins := finalizers(f.svc("web", "plain")); len(fins) != 0 {
		t.Fatalf("a Service without a class is MetalLB's in class-only mode: finalizers = %v", fins)
	}
	ing := ingress(f.svc("web", "proxy"))
	if len(ing) != 1 || ing[0].(map[string]any)["ip"] != "203.0.113.20" || ing[0].(map[string]any)["ipMode"] != "Proxy" {
		t.Fatalf("proxy ingress = %v", ing)
	}
	if ing := ingress(f.svc("web", "classed")); len(ing) != 0 {
		t.Fatalf("a building Service has no address: %v", ing)
	}
	want := []string{"classed:EnsuringLoadBalancer", "udp:UDPNotSupported", "proxy:EnsuredLoadBalancer", "proxy:LoadBalancerIPIgnored"}
	if got := reasons(f.events); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if ev := f.events[1]; ev["type"] != "Warning" || ev["message"] != "TCP only" || ev["involvedObject"].(map[string]any)["uid"] != "u-5" {
		t.Fatalf("refusal event = %v", ev)
	}

	// A standing state is not reported again, and an unchanged status is not written again.
	if err := c.turn(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 4 {
		t.Fatalf("a second turn added events: %v", reasons(f.events[4:]))
	}

	// The address comes: status VIP, one Event.
	g.answer = func(s lbgw.Service) lbgw.ServiceStatus {
		if s.UID == "u-5" {
			return lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateRefused, Reason: lbgw.ReasonUDPNotSupported, Message: "TCP only"}
		}
		return lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: "203.0.113." + s.UID[2:]}
	}
	if err := c.turn(ctx); err != nil {
		t.Fatal(err)
	}
	if ing := ingress(f.svc("web", "classed")); len(ing) != 1 || ing[0].(map[string]any)["ip"] != "203.0.113.1" || ing[0].(map[string]any)["ipMode"] != "VIP" {
		t.Fatalf("classed ingress = %v", ing)
	}

	// Deleted: held until a PUT without it is accepted, then let go (and gone). Type changed: let go, status cleared.
	f.svc("web", "classed")["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-21T13:00:00Z"
	f.svc("web", "proxy")["spec"] = map[string]any{"type": "ClusterIP", "ports": []any{tcp(80, 0)}}
	g.refusal = &lbgw.Refusal{Status: http.StatusServiceUnavailable, Message: "lb gateway: try again"}
	if err := c.turn(ctx); err == nil {
		t.Fatal("a refused PUT is an error")
	}
	if f.svc("web", "classed") == nil || !slices.Equal(finalizers(f.svc("web", "proxy")), []string{Finalizer}) {
		t.Fatal("finalizers went while the gateway refused")
	}
	if got := reasons(f.events[len(f.events)-1:]); got[0] != "udp:GatewayRefused" {
		t.Fatalf("a whole refusal is reported on the owned Services: %v", got)
	}
	g.refusal = nil
	if err := c.turn(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := uids(g.last), []string{"u-5"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	if f.svc("web", "classed") != nil {
		t.Fatal("the deleted Service kept its finalizer")
	}
	if p := f.svc("web", "proxy"); len(finalizers(p)) != 0 || len(ingress(p)) != 0 {
		t.Fatalf("a Service no longer of type LoadBalancer: finalizers %v, ingress %v", finalizers(p), ingress(p))
	}

	// Default mode (a new cluster): Services without a class too, never proxcloud.io/internal.
	c = &controller{api: f, put: g.put, defaultClass: true, log: quiet()}
	if err := c.turn(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := uids(g.last), []string{"u-2", "u-5"}; !slices.Equal(got, want) {
		t.Fatalf("default mode sent %v, want %v", got, want)
	}
	if fins := finalizers(f.svc("web", "internal")); len(fins) != 0 {
		t.Fatalf("MetalLB's class is never owned: %v", fins)
	}
}

// A finalizer that cannot be written (the Service changed since it was read) keeps the Service out of this turn's
// state: a delete in between would not wait for the portal.
func TestTurnSkipsAServiceNotHeld(t *testing.T) {
	f := &fakeTenant{services: map[string]map[string]any{}}
	f.add("web", "a", "u-1", "2026-11-21T10:00:00Z", lbSpec(Class, tcp(80, 30080)))
	g := &gatewayFake{answer: func(s lbgw.Service) lbgw.ServiceStatus {
		return lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateBuilding}
	}}
	c := &controller{api: &staleTenant{f}, put: g.put, log: quiet()}
	if err := c.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.last.Services) != 0 {
		t.Fatalf("sent %v", uids(g.last))
	}
}

type staleTenant struct{ *fakeTenant }

func (s *staleTenant) PatchService(context.Context, string, string, map[string]any) error {
	return &lbgw.Refusal{Status: http.StatusConflict, Message: "the object has been modified"}
}

// The gateway client: the credential header and protocol, the CA from the Secret, and a refusal as *lbgw.Refusal.
func TestGatewayPut(t *testing.T) {
	var gotAuth, gotProto string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotProto = r.Header.Get("Authorization"), r.Header.Get(lbgw.ProtocolHeader)
		if r.Method != http.MethodPut || r.URL.Path != lbgw.StatePath {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		var req lbgw.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Services) == 0 {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": lbgw.ReasonControllerTooOld, "message": "bump"})
			return
		}
		_ = json.NewEncoder(w).Encode(lbgw.Answer{Services: []lbgw.ServiceStatus{{UID: req.Services[0].UID, State: lbgw.StateReady, Address: "203.0.113.5"}}})
	}))
	defer ts.Close()
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("lb@proxcloud!pc-c-1=s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &gateway{url: ts.URL, dir: dir}
	ans, err := g.put(context.Background(), &lbgw.Request{Services: []lbgw.Service{{UID: "u-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "PVEAPIToken=lb@proxcloud!pc-c-1=s3cr3t" || gotProto != strconv.Itoa(lbgw.Protocol) {
		t.Fatalf("headers %q %q", gotAuth, gotProto)
	}
	if len(ans.Services) != 1 || ans.Services[0].Address != "203.0.113.5" {
		t.Fatalf("answer = %+v", ans)
	}
	_, err = g.put(context.Background(), &lbgw.Request{})
	rf, ok := err.(*lbgw.Refusal)
	if !ok || rf.Status != http.StatusConflict || rf.Reason != lbgw.ReasonControllerTooOld || rf.Message != "bump" {
		t.Fatalf("refusal = %#v", err)
	}
	// A Secret without a CA certificate: no call.
	_ = os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a certificate"), 0o600)
	if _, err := g.put(context.Background(), &lbgw.Request{Services: []lbgw.Service{{UID: "u-1"}}}); err == nil {
		t.Fatal("a call without a CA went out")
	}
}
