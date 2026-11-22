package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
		t.Errorf("selector by OpenStack's name: %+v", s[1])
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
	// OpenStack's annotation is not the controller's to read: the portal said Proxy.
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

// A portal older than the controller (a portal rolled back to v0.11.0) decodes the request strictly and refuses a
// field it does not know. It says its protocol on every answer, and the next request is the one protocol 1 had.
func TestGatewayPutFallsBackToProtocol1(t *testing.T) {
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
	var got []oldService
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(lbgw.ProtocolHeader, "1")
		var req struct {
			Services []oldService
			Nodes    []lbgw.Node
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"reason": lbgw.ReasonInvalid, "message": "lb gateway: the body is not a state: " + err.Error()})
			return
		}
		got = req.Services
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
	req := &lbgw.Request{Services: []lbgw.Service{{UID: "u-1", Ports: []lbgw.Port{{Protocol: "TCP", Port: 80, NodePort: 30080}}, ProxyProtocol: true,
		Annotations: map[string]string{"proxcloud.io/lb-proxy-protocol": "true"}, SessionAffinity: "ClientIP", SessionAffinityTimeout: 600, Nodes: &[]string{"10.0.0.11"}}}}
	if _, err := g.put(context.Background(), req); err == nil {
		t.Fatal("the old portal took a field it does not know: the test's portal is not v0.11.0's")
	}
	ans, err := g.put(context.Background(), req)
	if err != nil || len(ans.Services) != 1 || ans.Services[0].Address != "203.0.113.5" {
		t.Fatalf("second request: %+v %v", ans, err)
	}
	if len(got) != 1 || !got[0].ProxyProtocol || got[0].Ports[0].NodePort != 30080 {
		t.Fatalf("the old portal got %+v", got)
	}
	if req.Services[0].Annotations == nil || req.Services[0].Nodes == nil {
		t.Fatal("the caller's request was changed")
	}
}
