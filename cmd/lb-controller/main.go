// Command lb-controller is the load balancer controller of one managed Kubernetes cluster. It runs on the platform
// cluster next to the cluster's control plane (chart proxmox-kamaji-cluster, templates/lb-controller.yaml), reads the
// tenant API through the Kamaji admin kubeconfig, and every ten seconds PUTs the cluster's whole load balancer state
// to the portal's gateway (internal/cloud/lbgw) with the cluster's own credential. It owns the Services of type
// LoadBalancer of class proxcloud.io/lb, and those without a class when started with -default; it holds them with the
// finalizer proxcloud.io/load-balancer, writes their status.loadBalancer and turns the portal's answers into Events.
// A poll, not a watch: a full list each turn means a PUT never comes from a half-filled cache. See docs/managed-k8s.md.
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
	proxyAnnotation = "proxcloud.io/lb-proxy-protocol"
	excludeLabel    = "node.kubernetes.io/exclude-from-external-load-balancers"
	component       = "proxcloud-lb-controller"
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
type gateway struct{ url, dir string }

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
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(g.url, "/")+lbgw.StatePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "PVEAPIToken="+strings.TrimSpace(string(token)))
	r.Header.Set(lbgw.ProtocolHeader, strconv.Itoa(lbgw.Protocol))
	r.Header.Set("Content-Type", "application/json")
	res, err := client.Do(r)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var rf struct{ Reason, Message string }
		_ = json.Unmarshal(raw, &rf)
		if rf.Message == "" {
			rf.Message = fmt.Sprintf("the gateway answered %d", res.StatusCode)
		}
		return nil, &lbgw.Refusal{Status: res.StatusCode, Reason: rf.Reason, Message: rf.Message}
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
}

// service is what a turn reads of one Service.
type service struct {
	ns, name, uid, rv, created string
	finalizers                 []string
	owned, deleting, proxy     bool
	loadBalancerIP             string
	ingress                    []any
	wire                       lbgw.Service
}

func (s *service) held() bool { return slices.Contains(s.finalizers, Finalizer) }

// turn lists everything, holds the owned Services, PUTs the whole state, writes each Service's status and Events, and
// lets go of the Services the state no longer has (deleted, no longer of type LoadBalancer, or of another class).
func (c *controller) turn(ctx context.Context) error {
	if c.events == nil {
		c.events = map[string]string{}
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
	req := &lbgw.Request{Services: []lbgw.Service{}, Nodes: readNodes(nodeItems)}
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
			continue
		}
		c.setStatus(ctx, s, st.Address)
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
		if err := c.setFinalizers(ctx, s, slices.DeleteFunc(slices.Clone(s.finalizers), func(f string) bool { return f == Finalizer })); err != nil {
			c.log.Warn("lb-controller: remove finalizer", "service", s.ns+"/"+s.name, "err", err)
			continue
		}
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
		ExternalTrafficPolicy: str(spec["externalTrafficPolicy"]), HealthCheckNodePort: num(spec["healthCheckNodePort"])}
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

// readNodes lists the nodes' IPv4 InternalIP, without the nodes labelled to stay out of external load balancers and
// those being deleted. Whether a node serves is the balancer's health check, not this list.
func readNodes(items []map[string]any) []lbgw.Node {
	out := []lbgw.Node{}
	for _, n := range items {
		meta, _ := n["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		if _, excluded := labels[excludeLabel]; excluded || meta["deletionTimestamp"] != nil {
			continue
		}
		status, _ := n["status"].(map[string]any)
		for _, a := range list(status["addresses"]) {
			m, _ := a.(map[string]any)
			if ip, err := netip.ParseAddr(str(m["address"])); err == nil && ip.Is4() && str(m["type"]) == "InternalIP" {
				out = append(out, lbgw.Node{Name: str(meta["name"]), Address: ip.String()})
				break
			}
		}
	}
	return out
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
