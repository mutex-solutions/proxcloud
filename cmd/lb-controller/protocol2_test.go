package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
)

func udp(port, nodePort int) map[string]any {
	return map[string]any{"protocol": "UDP", "port": float64(port), "nodePort": float64(nodePort)}
}

func annotate(s map[string]any, kv ...string) {
	ann := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		ann[kv[i]] = kv[i+1]
	}
	s["metadata"].(map[string]any)["annotations"] = ann
}

func taint(n map[string]any, key string) map[string]any {
	n["spec"] = map[string]any{"taints": []any{map[string]any{"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule"}, map[string]any{"key": key, "effect": "NoSchedule"}}}
	return n
}

func ready2(g *gatewayFake) {
	g.answer = func(s lbgw.Service) lbgw.ServiceStatus {
		return lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: "203.0.113.5", IPMode: "VIP"}
	}
}

// Upstream's node rule (k8s.io/cloud-provider controllers/service/controller.go, stableNodeSetPredicates): the
// exclusion label takes a node out unless its value is a false boolean, the cluster autoscaler's taint takes it out,
// a node being deleted is out, and neither a cordon nor NotReady takes a member out.
func TestNodeRuleOfUpstream(t *testing.T) {
	label := func(v string) map[string]any { return map[string]any{excludeLabel: v} }
	deleting := node("w-deleting", "10.0.0.30", nil)
	deleting["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-23T10:00:00Z"
	cordoned := node("w-cordoned", "10.0.0.31", nil)
	cordoned["spec"] = map[string]any{"unschedulable": true, "taints": []any{map[string]any{"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule"}}}
	items := []map[string]any{
		node("w-plain", "10.0.0.11", nil),
		node("w-empty", "10.0.0.12", label("")),      // ParseBool fails: out
		node("w-true", "10.0.0.13", label("true")),   // out
		node("w-false", "10.0.0.14", label("false")), // in: the label says "do not exclude"
		node("w-zero", "10.0.0.15", label("0")),      // in
		node("w-one", "10.0.0.16", label("1")),       // out
		node("w-word", "10.0.0.17", label("yes")),    // not a boolean: out
		node("w-False", "10.0.0.18", label("False")), // in
		taint(node("w-scaledown", "10.0.0.19", nil), toBeDeletedTaint),
		taint(node("w-othertaint", "10.0.0.20", nil), "example.com/dedicated"),
		deleting,
		cordoned,
		ready(node("w-notready", "10.0.0.32", nil), "False"),
	}
	want := []string{"10.0.0.11", "10.0.0.14", "10.0.0.15", "10.0.0.18", "10.0.0.20", "10.0.0.31", "10.0.0.32"}
	// A controller that just started takes every node upstream's rule lets in, Ready or not.
	nodes, labels, members := readNodes(items, nil)
	if got := addressesOf(nodes); !slices.Equal(got, want) {
		t.Fatalf("members %v, want %v", got, want)
	}
	if labels["10.0.0.14"][excludeLabel] != "false" || len(labels) != len(want) {
		t.Fatalf("labels by address: %v", labels)
	}

	// The two rules together. A node that is new to a running controller waits for its first Ready (ours); once a
	// member it stays through NotReady (upstream's); the taint takes it out at once (upstream's), and when the taint
	// goes it starts over, waiting for Ready again (ours).
	fresh := ready(node("w-new", "10.0.0.40", nil), "False")
	step := func(n map[string]any) bool {
		var out []lbgw.Node
		out, _, members = readNodes([]map[string]any{items[0], n}, members)
		return slices.Contains(addressesOf(out), "10.0.0.40")
	}
	if step(fresh) {
		t.Fatal("a node new to the balancer is a member before it is Ready")
	}
	if !step(ready(fresh, "True")) || !step(ready(fresh, "Unknown")) {
		t.Fatal("a member that went NotReady left: its health check is to decide")
	}
	if step(taint(fresh, toBeDeletedTaint)) {
		t.Fatal("a node the autoscaler is about to delete stayed a member")
	}
	delete(fresh, "spec")
	if step(fresh) {
		t.Fatal("a node whose taint went is a member again before it is Ready: it starts over")
	}
	if !step(ready(fresh, "True")) {
		t.Fatal("a node whose taint went and that is Ready is not a member")
	}
}

// What a protocol 2 controller sends of a Service: its annotations of the load balancer's names as they are, its
// affinity, and behind a node selector (by either name) only the nodes it selects.
func TestServiceWireOfProtocol2(t *testing.T) {
	ctx := context.Background()
	f := &fakeTenant{services: map[string]map[string]any{}}
	dns := lbSpec(Class, tcp(53, 30053), udp(53, 30053))
	dns["sessionAffinity"] = "ClientIP"
	dns["sessionAffinityConfig"] = map[string]any{"clientIP": map[string]any{"timeoutSeconds": float64(600)}}
	f.add("kube-system", "dns", "u-1", "2026-11-23T10:00:00Z", dns)
	annotate(f.svc("kube-system", "dns"), "proxcloud.io/lb-node-selector", "pool=edge, public",
		"loadbalancer.openstack.org/lb-method", "SOURCE_IP", "service.beta.kubernetes.io/openstack-internal-load-balancer", "false",
		"kubectl.kubernetes.io/last-applied-configuration", "{}", "proxcloud.io/owner", "me")
	web := lbSpec(Class, tcp(80, 30080))
	web["sessionAffinity"] = "None"
	f.add("web", "alias", "u-2", "2026-11-23T10:01:00Z", web)
	annotate(f.svc("web", "alias"), "loadbalancer.openstack.org/node-selector", "pool=core")
	f.add("web", "nowhere", "u-3", "2026-11-23T10:02:00Z", lbSpec(Class, tcp(80, 30081)))
	annotate(f.svc("web", "nowhere"), "proxcloud.io/lb-node-selector", "pool=gone")
	f.add("web", "plain", "u-4", "2026-11-23T10:03:00Z", lbSpec(Class, tcp(80, 30082)))
	f.nodes = []map[string]any{
		node("w1", "10.0.0.11", map[string]any{"pool": "edge", "public": ""}),
		node("w2", "10.0.0.12", map[string]any{"pool": "edge"}),
		node("w3", "10.0.0.13", map[string]any{"pool": "core", "public": "yes"}),
		node("w4", "10.0.0.14", nil),
	}
	g := &gatewayFake{}
	ready2(g)
	c := &controller{api: f, put: g.put, log: quiet()}
	if err := c.turn(ctx); err != nil {
		t.Fatal(err)
	}
	s := g.last.Services
	if len(s) != 4 || len(g.last.Nodes) != 4 {
		t.Fatalf("sent %+v", g.last)
	}
	wantAnn := map[string]string{"proxcloud.io/lb-node-selector": "pool=edge, public", "loadbalancer.openstack.org/lb-method": "SOURCE_IP",
		"service.beta.kubernetes.io/openstack-internal-load-balancer": "false"}
	if len(s[0].Annotations) != len(wantAnn) {
		t.Fatalf("annotations sent: %v", s[0].Annotations)
	}
	for k, v := range wantAnn {
		if s[0].Annotations[k] != v {
			t.Errorf("annotation %s = %q", k, s[0].Annotations[k])
		}
	}
	if s[0].SessionAffinity != "ClientIP" || s[0].SessionAffinityTimeout != 600 || len(s[0].Ports) != 2 || s[0].Ports[1].Protocol != "UDP" {
		t.Errorf("dns: %+v", s[0])
	}
	if s[0].Nodes == nil || !slices.Equal(*s[0].Nodes, []string{"10.0.0.11"}) {
		t.Errorf("selector pool=edge, public: %v", s[0].Nodes)
	}
	if s[1].Nodes == nil || !slices.Equal(*s[1].Nodes, []string{"10.0.0.13"}) || s[1].SessionAffinity != "" {
		t.Errorf("selector by OCCM's name: %+v", s[1])
	}
	if s[2].Nodes == nil || len(*s[2].Nodes) != 0 {
		t.Errorf("a selector that matches no node is an empty list, not none: %v", s[2].Nodes)
	}
	if s[3].Nodes != nil || s[3].Annotations != nil {
		t.Errorf("a Service without annotations: %+v", s[3])
	}
	raw, _ := json.Marshal(s[2])
	if !strings.Contains(string(raw), `"nodes":[]`) {
		t.Errorf("on the wire: %s", raw)
	}
	if raw, _ = json.Marshal(s[3]); strings.Contains(string(raw), "nodes") || strings.Contains(string(raw), "annotations") || strings.Contains(string(raw), "sessionAffinity") {
		t.Errorf("a Service that asks for nothing new is the request of protocol 1: %s", raw)
	}
	// No node behind a Service: upstream's warning, once.
	if got := reasons(f.events); !slices.Contains(got, "nowhere:UnAvailableLoadBalancer") || slices.Contains(got, "plain:UnAvailableLoadBalancer") {
		t.Errorf("events %v", got)
	}
}

// The Events: upstream's for a changed node set, for no node, for a sync that failed and around the delete; the
// portal's for the annotations, each once while it stands; and the ipMode the portal answers.
func TestEventsOfProtocol2(t *testing.T) {
	ctx := context.Background()
	f := &fakeTenant{services: map[string]map[string]any{}}
	f.add("web", "shop", "u-1", "2026-11-23T10:00:00Z", lbSpec(Class, tcp(80, 30080)))
	annotate(f.svc("web", "shop"), "loadbalancer.openstack.org/proxy-protocol", "true", "loadbalancer.openstack.org/flavor-id", "f1")
	f.nodes = []map[string]any{node("w1", "10.0.0.11", nil)}
	notSupported := lbgw.Event{Type: "Warning", Reason: lbgw.ReasonAnnotationNotSupported, Message: "The load balancer does not obey these annotations: loadbalancer.openstack.org/flavor-id"}
	events := []lbgw.Event{notSupported}
	g := &gatewayFake{answer: func(s lbgw.Service) lbgw.ServiceStatus {
		return lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: "203.0.113.5", IPMode: "Proxy", Events: events}
	}}
	c := &controller{api: f, put: g.put, log: quiet()}
	turn := func() []string {
		t.Helper()
		n := len(f.events)
		if err := c.turn(ctx); err != nil {
			t.Fatal(err)
		}
		return reasons(f.events[n:])
	}
	if got, want := turn(), []string{"shop:EnsuredLoadBalancer", "shop:AnnotationNotSupported"}; !slices.Equal(got, want) {
		t.Fatalf("first turn: %v, want %v", got, want)
	}
	if ev := f.events[1]; ev["type"] != "Warning" || ev["message"] != notSupported.Message {
		t.Fatalf("the portal's Event: %v", ev)
	}
	// OCCM's annotation is not the controller's to read: the portal said Proxy.
	if ing := ingress(f.svc("web", "shop")); len(ing) != 1 || ing[0].(map[string]any)["ipMode"] != "Proxy" {
		t.Fatalf("ingress %v", ing)
	}
	if got := turn(); len(got) != 0 {
		t.Fatalf("a standing state said again: %v", got)
	}
	// A node joins: said once. The same set again: nothing.
	f.nodes = append(f.nodes, node("w2", "10.0.0.12", nil))
	if got, want := turn(), []string{"shop:UpdatedLoadBalancer"}; !slices.Equal(got, want) {
		t.Fatalf("a node joined: %v", got)
	}
	if ev := f.events[len(f.events)-1]; ev["type"] != "Normal" || ev["message"] != "Updated load balancer with new hosts" {
		t.Fatalf("%v", ev)
	}
	if got := turn(); len(got) != 0 {
		t.Fatalf("an unchanged node set: %v", got)
	}
	// Every node gone: upstream's warning, once; back: a change, said again; and a second change is said again too.
	saved := f.nodes
	f.nodes = nil
	if got, want := turn(), []string{"shop:UnAvailableLoadBalancer"}; !slices.Equal(got, want) {
		t.Fatalf("no node: %v", got)
	}
	if ev := f.events[len(f.events)-1]; ev["type"] != "Warning" || ev["message"] != "There are no available nodes for LoadBalancer" {
		t.Fatalf("%v", ev)
	}
	if got := turn(); len(got) != 0 {
		t.Fatalf("still no node: %v", got)
	}
	f.nodes = saved
	if got, want := turn(), []string{"shop:UpdatedLoadBalancer"}; !slices.Equal(got, want) {
		t.Fatalf("nodes back: %v", got)
	}
	f.nodes = saved[:1]
	if got, want := turn(), []string{"shop:UpdatedLoadBalancer"}; !slices.Equal(got, want) {
		t.Fatalf("a node left: %v", got)
	}
	// The annotation goes and comes back: its Event comes again.
	events = nil
	if got := turn(); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	events = []lbgw.Event{notSupported}
	if got, want := turn(), []string{"shop:AnnotationNotSupported"}; !slices.Equal(got, want) {
		t.Fatalf("the Event of an annotation that came back: %v", got)
	}
	// The portal cannot be reached: upstream's word, once, with nothing of the error's text in it.
	g.refusal = errors.New(`Put "https://portal.internal:8444/lb/v1/state": dial tcp 10.1.2.3:8444: i/o timeout`)
	n := len(f.events)
	if err := c.turn(ctx); err == nil {
		t.Fatal("an unreachable gateway is an error")
	}
	_ = c.turn(ctx)
	if got := reasons(f.events[n:]); !slices.Equal(got, []string{"shop:SyncLoadBalancerFailed"}) {
		t.Fatalf("unreachable: %v", got)
	}
	if ev := f.events[n]; ev["type"] != "Warning" || strings.Contains(ev["message"].(string), "10.1.2.3") {
		t.Fatalf("%v", ev)
	}
	g.refusal = nil
	if got, want := turn(), []string{"shop:EnsuredLoadBalancer"}; !slices.Equal(got, want) {
		t.Fatalf("reachable again: %v", got)
	}
	// Deleted: the two Events around the finalizer.
	f.svc("web", "shop")["metadata"].(map[string]any)["deletionTimestamp"] = "2026-11-23T11:00:00Z"
	if got, want := turn(), []string{"shop:DeletingLoadBalancer", "shop:DeletedLoadBalancer"}; !slices.Equal(got, want) {
		t.Fatalf("delete: %v", got)
	}
	if f.svc("web", "shop") != nil || len(c.hosts) != 0 {
		t.Fatalf("the Service is still there, or remembered: %v", c.hosts)
	}
}

// gatewayDir is the gateway Secret a controller mounts, for the test's portal.
func gatewayDir(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("lb@proxcloud!pc-c-1=s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A portal older than the controller (a portal rolled back to v0.11.0) decodes the request strictly and refuses a
// field it does not know, naming its protocol. The controller then sends it the request protocol 1 had, in the same
// turn and for that request only: the first request a portal that came back gets is the whole one again.
func TestGatewayPutStepsDownForOneRequestOnly(t *testing.T) {
	// v0.11.0's request, as its gateway decodes it (internal/cloud/lbgw/gateway.go at v0.11.0: DisallowUnknownFields).
	type oldService struct {
		UID, Namespace, Name  string
		Ports                 []lbgw.Port
		ExternalTrafficPolicy string
		HealthCheckNodePort   int
		SourceRanges          []string
		ProxyProtocol         bool
		IPFamilies            []string
	}
	var (
		old      = true        // the portal is v0.11.0
		refuse   bool          // the portal of this release refuses the request
		requests int           // since the test last looked
		got      []oldService  // what the old portal took
		whole    *lbgw.Request // what the portal of this release got
	)
	refusal := func(w http.ResponseWriter, msg string) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": lbgw.ReasonInvalid, "message": msg})
	}
	ready := func(w http.ResponseWriter, uid string) {
		_ = json.NewEncoder(w).Encode(lbgw.Answer{Services: []lbgw.ServiceStatus{{UID: uid, State: lbgw.StateReady, Address: "203.0.113.5"}}})
	}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if !old {
			w.Header().Set(lbgw.ProtocolHeader, "2")
			whole = &lbgw.Request{}
			if err := json.NewDecoder(r.Body).Decode(whole); err != nil || refuse {
				refusal(w, "lb gateway: a Service needs its uid, and Kubernetes names")
				return
			}
			ready(w, whole.Services[0].UID)
			return
		}
		w.Header().Set(lbgw.ProtocolHeader, "1")
		var req struct {
			Services []oldService
			Nodes    []lbgw.Node
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			refusal(w, "lb gateway: the body is not a state: "+err.Error())
			return
		}
		if len(req.Nodes) > lbgw.MaxNodes {
			refusal(w, "lb gateway: at most 64 nodes")
			return
		}
		got = req.Services
		ready(w, req.Services[0].UID)
	}))
	defer ts.Close()
	g := &gateway{url: ts.URL, dir: gatewayDir(t, ts)}
	req := &lbgw.Request{Services: []lbgw.Service{{UID: "u-1", Ports: []lbgw.Port{{Protocol: "TCP", Port: 80, NodePort: 30080}}, ProxyProtocol: true,
		Annotations: map[string]string{"proxcloud.io/lb-proxy-protocol": "true"}, SessionAffinity: "ClientIP", SessionAffinityTimeout: 600, Nodes: &[]string{"10.0.0.11"}}}}
	put := func() (*lbgw.Answer, error) {
		requests = 0
		return g.put(context.Background(), req)
	}

	// The old portal: served in the turn that finds it, with a second request.
	ans, err := put()
	if err != nil || len(ans.Services) != 1 || ans.Services[0].Address != "203.0.113.5" || requests != 2 {
		t.Fatalf("the old portal: %+v %v after %d requests", ans, err, requests)
	}
	if len(got) != 1 || !got[0].ProxyProtocol || got[0].Ports[0].NodePort != 30080 {
		t.Fatalf("the old portal got %+v", got)
	}
	if req.Services[0].Annotations == nil || req.Services[0].Nodes == nil {
		t.Fatal("the caller's request was changed")
	}
	// A Service that asks for an internal balancer is not sent to that portal at all: it reads no annotation and would
	// give the Service a public address.
	req.Services = append(req.Services, lbgw.Service{UID: "u-2", Ports: []lbgw.Port{{Protocol: "TCP", Port: 80, NodePort: 30081}},
		Annotations: map[string]string{lbgw.InternalAnnotation: "true"}})
	if _, err := put(); err != nil || len(got) != 1 || got[0].UID != "u-1" {
		t.Fatalf("an internal Service reached a portal of protocol 1: %+v %v", got, err)
	}
	if len(req.Services) != 2 {
		t.Fatal("the caller's request was changed")
	}
	// The old portal refuses the short request too: its refusal is the answer.
	for i := range lbgw.MaxNodes + 1 {
		req.Nodes = append(req.Nodes, lbgw.Node{Address: "10.0.1." + strconv.Itoa(i)})
	}
	var rf *lbgw.Refusal
	if _, err := put(); !errors.As(err, &rf) || rf.Status != http.StatusBadRequest || rf.Message != "lb gateway: at most 64 nodes" || requests != 2 {
		t.Fatalf("refused twice: %v after %d requests", err, requests)
	}
	req.Nodes = nil
	// A request with nothing of protocol 2 that the old portal refuses is not sent a second time.
	plain := &lbgw.Request{Services: []lbgw.Service{}, Nodes: make([]lbgw.Node, lbgw.MaxNodes+1)}
	requests = 0
	if _, err := g.put(context.Background(), plain); !errors.As(err, &rf) || requests != 1 {
		t.Fatalf("the same request went out again: %v after %d requests", err, requests)
	}

	// The portal is back on this release: its first request is the whole one, the internal Service too (this
	// portal refuses it itself).
	old = false
	if _, err := put(); err != nil || requests != 1 {
		t.Fatalf("the portal that came back: %v after %d requests", err, requests)
	}
	if first := whole.Services[0]; len(whole.Services) != 2 || first.Annotations["proxcloud.io/lb-proxy-protocol"] != "true" ||
		first.SessionAffinity != "ClientIP" || first.SessionAffinityTimeout != 600 || first.Nodes == nil || !slices.Equal(*first.Nodes, []string{"10.0.0.11"}) {
		t.Fatalf("the portal that came back was sent the request of protocol 1: %+v", whole.Services)
	}
	// What a portal of this release refuses is not tried again in the shorter form.
	refuse = true
	if _, err := put(); !errors.As(err, &rf) || rf.Status != http.StatusBadRequest || requests != 1 {
		t.Fatalf("a refusal of this release's portal: %v after %d requests", err, requests)
	}
}

// A node selector's terms: spaces around a term as OCCM drops them (StringToMap, its test " k1=v1, k2 "), and around
// "=" too, where OCCM's reading matches no node.
func TestParseSelector(t *testing.T) {
	for in, want := range map[string]map[string]string{
		"pool=edge,public":       {"pool": "edge", "public": ""},
		" pool=edge, public ":    {"pool": "edge", "public": ""},
		" pool = edge , public ": {"pool": "edge", "public": ""},
		"pool= edge":             {"pool": "edge"},
		"pool =edge":             {"pool": "edge"},
		"a=b=c":                  {"a": "b=c"},
		"public=":                {"public": ""},
		"":                       {},
		" , ":                    {},
		" = edge":                {},
	} {
		if got := parseSelector(in); !maps.Equal(got, want) {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}
