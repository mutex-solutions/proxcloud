// Command lb-controller is the load balancer controller of one managed Kubernetes cluster. It runs on the platform
// cluster next to the cluster's control plane (chart proxmox-kamaji-cluster, templates/lb-controller.yaml), reads the
// tenant API through the Kamaji admin kubeconfig, and every ten seconds PUTs the cluster's whole load balancer state
// to the portal's gateway (internal/cloud/lbgw) with the cluster's own credential. It owns the Services of type
// LoadBalancer of class proxcloud.io/lb, and those without a class when started with -default; it holds them with the
// finalizer proxcloud.io/load-balancer, writes their status.loadBalancer and turns the portal's answers into Events.
// A poll, not a watch: a full list each turn means a PUT never comes from a half-filled cache. See docs/managed-k8s.md.
//
// What a Service's annotations mean is the portal's to say (internal/cloud/lbgw/annotations.go): the controller sends
// them as they are and says the Events the answer carries. The one it reads itself is the node selector, which needs
// the nodes' labels.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
)

const (
	// Finalizer holds a Service in Terminating until the portal has a state without it.
	Finalizer = "proxcloud.io/load-balancer"
	// Class is the loadBalancerClass of the product balancer. MetalLB serves proxcloud.io/internal (default mode) or the
	// Services without a class (class-only mode).
	Class           = "proxcloud.io/lb"
	proxyAnnotation = lbgw.OwnPrefix + lbgw.KnobProxyProtocol
	excludeLabel    = "node.kubernetes.io/exclude-from-external-load-balancers"
	// toBeDeletedTaint is the cluster autoscaler's mark on a node it is about to remove.
	toBeDeletedTaint = "ToBeDeletedByClusterAutoscaler"
	component        = "proxcloud-lb-controller"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "/etc/tenant/kubeconfig", "the tenant cluster's admin kubeconfig (admin.svc)")
	gatewayURL := flag.String("gateway-url", "", "https://<host>:<port> of the portal's gateway listener")
	gatewayDir := flag.String("gateway-dir", "/etc/lb-gateway", "the gateway Secret: token (lb@proxcloud!<id>=<secret>) and ca.crt")
	defaultClass := flag.Bool("default", false, "also own the Services of type LoadBalancer without a class")
	interval := flag.Duration("interval", 10*time.Second, "time between two turns")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if !strings.HasPrefix(*gatewayURL, "https://") {
		log.Error("lb-controller: -gateway-url must be https://<host>:<port>")
		os.Exit(2)
	}
	kc, err := os.ReadFile(*kubeconfig)
	if err != nil {
		log.Error("lb-controller: kubeconfig", "err", err)
		os.Exit(1)
	}
	api, err := kamajik8s.New(string(kc))
	if err != nil {
		log.Error("lb-controller: kubeconfig", "err", err)
		os.Exit(1)
	}
	gw := &gateway{url: *gatewayURL, dir: *gatewayDir}
	c := &controller{api: api, put: gw.put, defaultClass: *defaultClass, log: log}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	log.Info("lb-controller: started", "gateway", *gatewayURL, "default", *defaultClass, "protocol", lbgw.Protocol)
	for {
		if err := c.turn(ctx); err != nil && ctx.Err() == nil {
			log.Warn("lb-controller: turn", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

// gateway is the portal's side. The token and the CA are read again for each call, so a rotation (the operator's
// "Enable load balancer controller" again) reaches a running controller when the kubelet updates the Secret volume.
type gateway struct {
	url, dir string
}

// protocol1 is req without what protocol 2 added, and without the Services that ask for an internal balancer: a
// portal of protocol 1 reads no annotation, so it would give such a Service a public address.
func protocol1(req *lbgw.Request) *lbgw.Request {
	out := &lbgw.Request{Services: []lbgw.Service{}, Nodes: req.Nodes}
	for _, s := range req.Services {
		if lbgw.Internal(s.Annotations) {
			continue
		}
		s.Annotations, s.SessionAffinity, s.SessionAffinityTimeout, s.Nodes = nil, "", 0, nil
		out.Services = append(out.Services, s)
	}
	return out
}

func (g *gateway) put(ctx context.Context, req *lbgw.Request) (*lbgw.Answer, error) {
	token, err := os.ReadFile(filepath.Join(g.dir, "token"))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(g.dir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("gateway: ca.crt holds no certificate")
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		// A new transport per call (a new CA may come); no idle connection outlives it.
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// send is one request: the answer's status, the protocol the portal says it speaks (0: it did not say) and
	// the answer's body.
	send := func(body []byte) (status, peer int, raw []byte, err error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(g.url, "/")+lbgw.StatePath, bytes.NewReader(body))
		if err != nil {
			return 0, 0, nil, err
		}
		r.Header.Set("Authorization", "PVEAPIToken="+strings.TrimSpace(string(token)))
		r.Header.Set(lbgw.ProtocolHeader, strconv.Itoa(lbgw.Protocol))
		r.Header.Set("Content-Type", "application/json")
		res, err := client.Do(r)
		if err != nil {
			return 0, 0, nil, err
		}
		defer res.Body.Close()
		peer, _ = strconv.Atoi(res.Header.Get(lbgw.ProtocolHeader))
		raw, err = io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return res.StatusCode, peer, raw, err
	}
	// The whole request first, every time. A portal older than this controller, which is a portal rolled back,
	// decodes strictly and refuses a field it does not know with 400, naming its protocol: only then, and for this
	// request only, it is sent the request protocol 1 had. Nothing is remembered: the first request a portal that
	// came back gets is the whole one, so no Service is served a turn without its options.
	// ponytail: while the portal is rolled back, a turn whose Services use protocol 2 costs two requests, and that
	// portal audits the refused one each turn (its audit is once per change, and the two answers alternate). A
	// rollback is short; remember the step-down, with a way for the newer portal to refuse the short request, if
	// one ever is not.
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	status, peer, raw, err := send(body)
	if err != nil {
		return nil, err
	}
	if status == http.StatusBadRequest && peer > 0 && peer < lbgw.Protocol {
		old, err := json.Marshal(protocol1(req))
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(old, body) { // the same request: the refusal is about something else
			if status, _, raw, err = send(old); err != nil {
				return nil, err
			}
		}
	}
	if status < 200 || status > 299 {
		var rf struct{ Reason, Message string }
		_ = json.Unmarshal(raw, &rf)
		if rf.Message == "" {
			rf.Message = fmt.Sprintf("the gateway answered %d", status)
		}
		return nil, &lbgw.Refusal{Status: status, Reason: rf.Reason, Message: rf.Message}
	}
	var ans lbgw.Answer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return nil, fmt.Errorf("gateway: answer: %w", err)
	}
	return &ans, nil
}

// tenantAPI is what a turn needs of the tenant cluster (kamajik8s.Client).
type tenantAPI interface {
	ListServicesAllNamespaces(ctx context.Context) ([]map[string]any, error)
	ListNodes(ctx context.Context) ([]map[string]any, error)
	PatchService(ctx context.Context, ns, name string, patch map[string]any) error
	PatchServiceStatus(ctx context.Context, ns, name string, patch map[string]any) error
	CreateCore(ctx context.Context, plural string, obj map[string]any) error
}

type controller struct {
	api          tenantAPI
	put          func(context.Context, *lbgw.Request) (*lbgw.Answer, error)
	defaultClass bool
	log          *slog.Logger
	// events is what the last Event of each Service (by UID) said, so a standing state is one Event, not one a turn.
	// ponytail: per process; a restart reports each Service's state once more.
	events map[string]string
	// members are the nodes sent so far, by name: a node is sent from its first turn Ready on (readNodes). nil until
	// the first list of nodes was read.
	// ponytail: per process; a controller that starts takes every node it finds for a member, so a node that joins
	// in that very turn is sent before it is Ready. Keep it in the portal's state if that ever matters.
	members map[string]bool
	// hosts are the nodes last sent behind each Service (by UID), for the Event that says they changed.
	hosts map[string]string
}

// service is what a turn reads of one Service.
type service struct {
	ns, name, uid, rv, created string
	finalizers                 []string
	owned, deleting, proxy     bool
	loadBalancerIP             string
	nodeSelector               string
	ingress                    []any
	wire                       lbgw.Service
}

func (s *service) held() bool { return slices.Contains(s.finalizers, Finalizer) }

// turn lists everything, holds the owned Services, PUTs the whole state, writes each Service's status and Events, and
// lets go of the Services the state no longer has (deleted, no longer of type LoadBalancer, or of another class).
func (c *controller) turn(ctx context.Context) error {
	if c.events == nil {
		c.events, c.hosts = map[string]string{}, map[string]string{}
	}
	items, err := c.api.ListServicesAllNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}
	nodeItems, err := c.api.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	services := make([]*service, 0, len(items))
	for _, it := range items {
		services = append(services, readService(it, c.defaultClass))
	}
	// The oldest first: when the balancer is full, the gateway refuses the newest.
	slices.SortFunc(services, func(a, b *service) int {
		return strings.Compare(a.created+a.uid, b.created+b.uid)
	})
	nodes, labels, members := readNodes(nodeItems, c.members)
	c.members = members
	req := &lbgw.Request{Services: []lbgw.Service{}, Nodes: nodes}
	hosts := map[string]string{} // by UID: the addresses behind the Service this turn
	var owned []*service
	for _, s := range services {
		if !s.owned || s.deleting {
			continue
		}
		if !s.held() {
			// Without the finalizer a delete would not wait for the portal: such a Service is sent from the next turn.
			if err := c.setFinalizers(ctx, s, append(slices.Clone(s.finalizers), Finalizer)); err != nil {
				c.log.Warn("lb-controller: add finalizer", "service", s.ns+"/"+s.name, "err", err)
				continue
			}
		}
		behind := addressesOf(nodes)
		if sel := parseSelector(s.nodeSelector); len(sel) > 0 {
			behind = slices.DeleteFunc(behind, func(a string) bool { return !selects(sel, labels[a]) })
			s.wire.Nodes = &behind
		}
		hosts[s.uid] = strings.Join(behind, ",")
		req.Services = append(req.Services, s.wire)
		owned = append(owned, s)
	}
	ans, err := c.put(ctx, req)
	if err != nil {
		var rf *lbgw.Refusal
		if errors.As(err, &rf) {
			reason := rf.Reason
			if reason == "" {
				reason = "GatewayRefused"
			}
			for _, s := range owned {
				c.report(ctx, s, "state", "Warning", reason, rf.Message)
			}
		} else {
			// The portal did not answer: upstream's word for a sync that failed (k8s.io/cloud-provider
			// controllers/service/controller.go, processServiceCreateOrUpdate).
			for _, s := range owned {
				c.report(ctx, s, "state", "Warning", "SyncLoadBalancerFailed", "Error syncing load balancer: the portal's gateway cannot be reached")
			}
		}
		return fmt.Errorf("gateway: %w", err)
	}
	answered := map[string]lbgw.ServiceStatus{}
	for _, st := range ans.Services {
		answered[st.UID] = st
	}
	for _, s := range owned {
		st, ok := answered[s.uid]
		if !ok {
			// Not sent: an internal Service and a portal of protocol 1 (protocol1). It has no address there.
			if lbgw.Internal(s.wire.Annotations) {
				if len(s.ingress) > 0 {
					c.setStatus(ctx, s, "")
				}
				c.report(ctx, s, "state", "Warning", lbgw.ReasonInternalNotSupported,
					"The load balancer's addresses are public: for an address inside the cluster's network set loadBalancerClass proxcloud.io/internal and remove "+lbgw.InternalAnnotation)
			}
			continue
		}
		// The address goes into the Service's status when the portal says it is served, not when it is allocated:
		// what waits for status.loadBalancer.ingress (helm --wait, Argo CD's health, external-dns) takes the Service
		// for reachable (drill N3-7: 3 min 12 s too early for a cluster's first Service). Kubernetes' own service
		// controller does the same: it patches the status with what the provider's EnsureLoadBalancer returned, and
		// a provider returns a status once its balancer is provisioned, an error before (k8s.io/cloud-provider
		// controllers/service/controller.go, syncLoadBalancerIfNeeded, v0.34.0). An address already written stays
		// while a later change is building, and goes when the portal names none (refused, or given back).
		if st.IPMode != "" { // protocol 2: the portal read the annotations, the alias too
			s.proxy = st.IPMode == "Proxy"
		}
		if st.State == lbgw.StateReady || st.Address == "" || hasIngress(s.ingress, st.Address) {
			c.setStatus(ctx, s, st.Address)
		}
		switch st.State {
		case lbgw.StateReady:
			c.report(ctx, s, "state", "Normal", "EnsuredLoadBalancer", "Public address "+st.Address+" on the cluster's load balancer")
		case lbgw.StateRefused:
			c.report(ctx, s, "state", "Warning", st.Reason, st.Message)
		default:
			msg := st.Message
			if msg == "" {
				msg = "The portal is building the cluster's load balancer"
			}
			c.report(ctx, s, "state", "Normal", "EnsuringLoadBalancer", msg)
		}
		if s.loadBalancerIP != "" {
			c.report(ctx, s, "ip", "Warning", "LoadBalancerIPIgnored",
				"spec.loadBalancerIP is ignored: the portal gives each Service an address of its own")
		}
		// What the Service's annotations raised, one Event per reason while it stands; a reason that is gone may
		// come again.
		said := map[string]bool{}
		for _, ev := range st.Events {
			said[s.uid+"/ev/"+ev.Reason] = true
			c.report(ctx, s, "ev/"+ev.Reason, ev.Type, ev.Reason, ev.Message)
		}
		for k := range c.events {
			if strings.HasPrefix(k, s.uid+"/ev/") && !said[k] {
				delete(c.events, k)
			}
		}
		if st.State == lbgw.StateRefused {
			delete(c.hosts, s.uid)
			continue
		}
		// The two Events of upstream's node sync (controller.go, lockedUpdateLoadBalancerHosts): no node at all is
		// a warning, a changed set is said once per change. The first set a controller sees is not a change.
		was, known := c.hosts[s.uid]
		c.hosts[s.uid] = hosts[s.uid]
		switch {
		case hosts[s.uid] == "":
			c.report(ctx, s, "nodes", "Warning", "UnAvailableLoadBalancer", "There are no available nodes for LoadBalancer")
		case known && was != hosts[s.uid]:
			delete(c.events, s.uid+"/nodes")
			c.report(ctx, s, "nodes", "Normal", "UpdatedLoadBalancer", "Updated load balancer with new hosts")
			delete(c.events, s.uid+"/nodes") // each change is said
		default:
			delete(c.events, s.uid+"/nodes")
		}
	}
	for _, n := range ans.Nodes {
		c.log.Warn("lb-controller: node refused", "node", n.Name, "address", n.Address, "reason", n.Reason)
	}
	// The PUT left these out, and the portal accepted it: let them go.
	for _, s := range services {
		if !s.held() || (s.owned && !s.deleting) {
			continue
		}
		if !s.deleting && len(s.ingress) > 0 {
			c.setStatus(ctx, s, "")
		}
		// Upstream's two Events around the finalizer (controller.go, processLoadBalancerDelete). "Deleted" is said
		// when the portal holds a state without the Service: its address leaves the appliances in the pass after.
		c.report(ctx, s, "delete", "Normal", "DeletingLoadBalancer", "Deleting load balancer")
		if err := c.setFinalizers(ctx, s, slices.DeleteFunc(slices.Clone(s.finalizers), func(f string) bool { return f == Finalizer })); err != nil {
			c.log.Warn("lb-controller: remove finalizer", "service", s.ns+"/"+s.name, "err", err)
			continue
		}
		c.report(ctx, s, "deleted", "Normal", "DeletedLoadBalancer", "Deleted load balancer")
		delete(c.hosts, s.uid)
		for k := range c.events {
			if strings.HasPrefix(k, s.uid+"/") {
				delete(c.events, k)
			}
		}
	}
	return nil
}

// readService reads one Service. Owned: type LoadBalancer and class proxcloud.io/lb, or no class in default mode.
func readService(obj map[string]any, defaultClass bool) *service {
	meta, _ := obj["metadata"].(map[string]any)
	spec, _ := obj["spec"].(map[string]any)
	status, _ := obj["status"].(map[string]any)
	lb, _ := status["loadBalancer"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	class := str(spec["loadBalancerClass"])
	s := &service{
		ns: str(meta["namespace"]), name: str(meta["name"]), uid: str(meta["uid"]), rv: str(meta["resourceVersion"]),
		created:        str(meta["creationTimestamp"]),
		deleting:       meta["deletionTimestamp"] != nil,
		owned:          str(spec["type"]) == "LoadBalancer" && (class == Class || (class == "" && defaultClass)),
		proxy:          str(ann[proxyAnnotation]) == "true",
		loadBalancerIP: str(spec["loadBalancerIP"]),
	}
	s.ingress, _ = lb["ingress"].([]any)
	for _, f := range list(meta["finalizers"]) {
		s.finalizers = append(s.finalizers, str(f))
	}
	w := lbgw.Service{UID: s.uid, Namespace: s.ns, Name: s.name, ProxyProtocol: s.proxy,
		ExternalTrafficPolicy: str(spec["externalTrafficPolicy"]), HealthCheckNodePort: num(spec["healthCheckNodePort"]),
		SessionAffinity: str(spec["sessionAffinity"])}
	if w.SessionAffinity == "None" {
		w.SessionAffinity = "" // the API server's default: nothing to say
	}
	if w.SessionAffinity == "ClientIP" {
		cfg, _ := spec["sessionAffinityConfig"].(map[string]any)
		clientIP, _ := cfg["clientIP"].(map[string]any)
		w.SessionAffinityTimeout = num(clientIP["timeoutSeconds"])
	}
	for k, v := range ann {
		if lbgw.SentAnnotation(k) {
			if w.Annotations == nil {
				w.Annotations = map[string]string{}
			}
			// A value the portal does not read (lbgw.Read) is sent cut, one character over the limit: the portal
			// still names it in the Service's Event, and one long annotation cannot take the cluster's whole state
			// over the gateway's body limit.
			val := str(v)
			if len(val) > lbgw.MaxAnnotationValue {
				val = val[:lbgw.MaxAnnotationValue+1]
			}
			w.Annotations[k] = val
		}
	}
	read, _ := lbgw.Read(w.Annotations) // a selector the portal says it does not obey is not obeyed here either
	s.nodeSelector, _, _ = lbgw.Annotation(read, lbgw.KnobNodeSelector)
	for _, p := range list(spec["ports"]) {
		m, _ := p.(map[string]any)
		w.Ports = append(w.Ports, lbgw.Port{Name: str(m["name"]), Protocol: str(m["protocol"]), Port: num(m["port"]), NodePort: num(m["nodePort"])})
	}
	for _, r := range list(spec["loadBalancerSourceRanges"]) {
		w.SourceRanges = append(w.SourceRanges, str(r))
	}
	for _, f := range list(spec["ipFamilies"]) {
		w.IPFamilies = append(w.IPFamilies, str(f))
	}
	s.wire = w
	return s
}

// readNodes lists the nodes' IPv4 InternalIP, and their labels by that address for a Service's node selector.
// members is the controller's memory of the nodes it sent, which readNodes keeps.
//
// Two rules decide, and both must hold.
//
// Upstream's (k8s.io/cloud-provider controllers/service/controller.go, stableNodeSetPredicates with
// nodeNotDeletedPredicate, nodeIncludedPredicate and nodeUnTaintedPredicate; KEP-3458): a node is out while it is
// being deleted, while it carries the label node.kubernetes.io/exclude-from-external-load-balancers with a value
// that is not a false boolean (strconv.ParseBool: "false", "0", "f" keep the node in; "true", the empty value and
// anything that does not parse take it out), and while it carries the cluster autoscaler's taint
// ToBeDeletedByClusterAutoscaler. Ready is not part of it: a node that is NotReady or cordoned stays, and the
// balancer's health check decides, because a Ready condition that flaps must not reconfigure every balancer and a
// node whose kubelet is silent may still forward.
//
// Ours, on top, for a node the balancer does not have yet: it is sent from its first turn Ready on. HAProxy starts a
// member new to its config as UP ("init-state down" needs 3.1, Debian 13 ships 3.0), so a node sent before it is
// Ready took its share of the requests until its first check had failed (drill N3-8 of v0.11.0: 4.4 s, 22 of 44
// requests, at each worker added or replaced). So the two combine as: upstream's rule says who may be a member, and
// a node that may be one becomes one at its first Ready and then stays one, Ready or not, until upstream's rule
// takes it out. A node taken out starts over, and waits for Ready again.
//
// members nil is a controller that just started: it does not know what the balancer has, so every node upstream's
// rule lets in counts as a member, as that controller would have it. Waiting for Ready there would take a member
// that is NotReady at that moment out of a balancer it may still serve through, and all of them when no kubelet
// reaches the control plane while the controller is restarted.
func readNodes(items []map[string]any, members map[string]bool) ([]lbgw.Node, map[string]map[string]any, map[string]bool) {
	started := members == nil
	if started {
		members = map[string]bool{}
	}
	out := []lbgw.Node{}
	labelsOf := map[string]map[string]any{}
	seen := map[string]bool{}
	for _, n := range items {
		meta, _ := n["metadata"].(map[string]any)
		spec, _ := n["spec"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		name := str(meta["name"])
		if meta["deletionTimestamp"] != nil || excluded(labels) || tainted(spec) {
			continue
		}
		status, _ := n["status"].(map[string]any)
		if !started && !members[name] && !nodeReady(status) {
			continue
		}
		members[name], seen[name] = true, true
		for _, a := range list(status["addresses"]) {
			m, _ := a.(map[string]any)
			if ip, err := netip.ParseAddr(str(m["address"])); err == nil && ip.Is4() && str(m["type"]) == "InternalIP" {
				out = append(out, lbgw.Node{Name: name, Address: ip.String()})
				labelsOf[ip.String()] = labels
				break
			}
		}
	}
	for name := range members { // gone, deleting, excluded or tainted: a node of that name starts over
		if !seen[name] {
			delete(members, name)
		}
	}
	return out, labelsOf, members
}

// excluded is upstream's nodeIncludedPredicate, turned round: the exclusion label is there and its value is not a
// false boolean.
func excluded(labels map[string]any) bool {
	v, has := labels[excludeLabel]
	if !has {
		return false
	}
	off, err := strconv.ParseBool(str(v))
	return err != nil || off
}

// tainted is upstream's nodeUnTaintedPredicate, turned round.
func tainted(spec map[string]any) bool {
	return slices.ContainsFunc(list(spec["taints"]), func(t any) bool {
		m, _ := t.(map[string]any)
		return str(m["key"]) == toBeDeletedTaint
	})
}

func addressesOf(nodes []lbgw.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Address)
	}
	return out
}

// parseSelector reads a node selector annotation as OCCM does ("key=value, key":
// OCCM v1.37.1 loadbalancer.go:467-470, pkg/util/util.go:73-94 StringToMap, and matchNodeLabels,
// loadbalancer.go:2360-2373): terms between commas, each a label that must have that value, or without "=" a label
// that must be there. Spaces around a term are dropped, as StringToMap drops them.
//
// Spaces around "=" are dropped too, which StringToMap does not do: it reads "pool = edge" as the key "pool " and
// the value " edge", which no node has, and the Service is left without a member. A label's key and value hold no
// space, so the term can only mean pool=edge, and a selector OCCM matches nodes with matches the same nodes here.
func parseSelector(v string) map[string]string {
	out := map[string]string{}
	for _, term := range strings.Split(v, ",") {
		k, val, _ := strings.Cut(term, "=")
		if k = strings.TrimSpace(k); k != "" {
			out[k] = strings.TrimSpace(val)
		}
	}
	return out
}

// selects: the node's labels hold every term of the selector.
func selects(sel map[string]string, labels map[string]any) bool {
	for k, want := range sel {
		got, has := labels[k]
		if !has || (want != "" && str(got) != want) {
			return false
		}
	}
	return true
}

// nodeReady: the node's Ready condition is True.
func nodeReady(status map[string]any) bool {
	for _, c := range list(status["conditions"]) {
		if m, _ := c.(map[string]any); str(m["type"]) == "Ready" {
			return str(m["status"]) == "True"
		}
	}
	return false
}

// hasIngress: the Service's status already names address.
func hasIngress(ingress []any, address string) bool {
	return slices.ContainsFunc(ingress, func(i any) bool { m, _ := i.(map[string]any); return str(m["ip"]) == address })
}

// setFinalizers writes the Service's finalizers, at the resourceVersion it was read at.
func (c *controller) setFinalizers(ctx context.Context, s *service, fins []string) error {
	if fins == nil {
		fins = []string{}
	}
	if err := c.api.PatchService(ctx, s.ns, s.name, map[string]any{"metadata": map[string]any{"finalizers": fins, "resourceVersion": s.rv}}); err != nil {
		return err
	}
	s.finalizers = fins
	return nil
}

// setStatus writes status.loadBalancer.ingress when it differs: the address, ipMode Proxy with the PROXY header (so
// in-cluster clients go through the balancer and get the header too), VIP otherwise. No address: no ingress.
func (c *controller) setStatus(ctx context.Context, s *service, address string) {
	var want []any
	if address != "" {
		mode := "VIP"
		if s.proxy {
			mode = "Proxy"
		}
		want = []any{map[string]any{"ip": address, "ipMode": mode}}
	}
	if sameIngress(s.ingress, want) {
		return
	}
	var ingress any = want
	if want == nil {
		ingress = nil // a merge patch removes the key
	}
	if err := c.api.PatchServiceStatus(ctx, s.ns, s.name, map[string]any{"status": map[string]any{"loadBalancer": map[string]any{"ingress": ingress}}}); err != nil {
		c.log.Warn("lb-controller: status", "service", s.ns+"/"+s.name, "err", err)
		return
	}
	s.ingress = want
}

func sameIngress(have, want []any) bool {
	if len(have) != len(want) {
		return false
	}
	for i := range have {
		h, _ := have[i].(map[string]any)
		w, _ := want[i].(map[string]any)
		if str(h["ip"]) != str(w["ip"]) || str(h["ipMode"]) != str(w["ipMode"]) {
			return false
		}
	}
	return true
}

// report creates an Event on the Service unless its last Event in slot ("state", "ip") said the same.
func (c *controller) report(ctx context.Context, s *service, slot, typ, reason, msg string) {
	slot, key := s.uid+"/"+slot, reason+"|"+msg
	if c.events[slot] == key {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	ev := map[string]any{
		"apiVersion": "v1", "kind": "Event",
		"metadata": map[string]any{"generateName": s.name + ".", "namespace": s.ns},
		"involvedObject": map[string]any{"apiVersion": "v1", "kind": "Service", "namespace": s.ns, "name": s.name,
			"uid": s.uid, "resourceVersion": s.rv},
		"type": typ, "reason": reason, "message": msg, "count": 1,
		"firstTimestamp": now, "lastTimestamp": now,
		"source": map[string]any{"component": component},
	}
	if err := c.api.CreateCore(ctx, "events", ev); err != nil {
		c.log.Warn("lb-controller: event", "service", s.ns+"/"+s.name, "reason", reason, "err", err)
		return
	}
	c.events[slot] = key
}

func str(v any) string { s, _ := v.(string); return s }

func list(v any) []any { l, _ := v.([]any); return l }

// num reads a JSON number (float64 from encoding/json).
func num(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}
