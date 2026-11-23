// Command lb-controller is the load balancer controller of one managed Kubernetes cluster. It runs on the platform
// cluster next to the cluster's control plane (chart proxmox-kamaji-cluster, templates/lb-controller.yaml), reads the
// tenant API through the Kamaji admin kubeconfig, and PUTs the cluster's whole load balancer state to the portal's
// gateway (internal/cloud/lbgw) with the cluster's own credential. It owns the Services of type
// LoadBalancer of class proxcloud.io/lb, and those without a class when started with -default; it holds them with the
// finalizer proxcloud.io/load-balancer, writes their status.loadBalancer and turns the portal's answers into Events.
// Every turn lists everything: a PUT never comes from a half-filled cache. With -watch (chart 0.1.19) a watch of the
// Services, the Nodes and the Secrets the Services name says when to take a turn, and says nothing else (run);
// without it a turn comes every -interval, as under charts 0.1.17 and 0.1.18. See docs/managed-k8s.md.
//
// What a Service's annotations mean is the portal's to say (internal/cloud/lbgw/annotations.go): the controller sends
// the ones of the prefix proxcloud.io/lb-, and no other, as they are and says the Events the answer carries. The two
// it reads itself need the tenant's API: the node selector (the nodes' labels) and proxcloud.io/lb-tls-secret (the
// Secrets a Service's TLS ends with: certificates, below).
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
	"maps"
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
	"unicode/utf8"

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
	// sentEvery is the least time between two sends of one Secret to the portal.
	sentEvery = time.Minute
	// resyncEvery is the longest a state that did not change goes unsent: the portal hears from a controller that
	// is there, and what a turn could not see as a change is put right. debounce is how long a turn waits after a
	// change, for the changes that come with it.
	resyncEvery = 30 * time.Second
	debounce    = time.Second
	component   = "proxcloud-lb-controller"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "/etc/tenant/kubeconfig", "the tenant cluster's admin kubeconfig (admin.svc)")
	gatewayURL := flag.String("gateway-url", "", "https://<host>:<port> of the portal's gateway listener")
	gatewayDir := flag.String("gateway-dir", "/etc/lb-gateway", "the gateway Secret: token (lb@proxcloud!<id>=<secret>) and ca.crt")
	defaultClass := flag.Bool("default", false, "also own the Services of type LoadBalancer without a class")
	interval := flag.Duration("interval", 10*time.Second, "time between two turns; with -watch, between two turns while the portal is still building or a turn failed")
	watch := flag.Bool("watch", false, "watch the cluster's Services, Nodes and the TLS Secrets its Services name, and take a turn within a second of a change")
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
	c := &controller{api: api, put: gw.put, defaultClass: *defaultClass, log: log,
		watching: *watch, interval: *interval, resync: resyncEvery, debounce: debounce}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	log.Info("lb-controller: started", "gateway", *gatewayURL, "default", *defaultClass, "protocol", lbgw.Protocol, "watch", *watch)
	c.run(ctx)
}

// run takes turn after turn until ctx ends. A turn is always the whole of it: list everything, send the whole state.
// What differs is when the next one comes.
//
// Watching: within debounce of a change of a Service, a Node or a Secret a Service names, and at the latest when the
// state last sent is resync old. The watches say only that something changed (kamajik8s.WatchServices): an event
// that is lost, a stream that is down or an API server that forbids the watch costs time, never a wrong state.
// While a turn left something open (the portal is building, a certificate is asked for, a write failed), and after
// a turn that failed, the next one comes after interval as without a watch: the portal's answer changes without
// anything changing in the cluster.
//
// Not watching: every interval.
//
// ponytail: every event of a Node is a turn, a kubelet's heartbeat too (one per node every five minutes), and a turn
// lists every Node. With hundreds of nodes the heartbeats alone are a turn a second, which the debounce caps there.
// If a cluster of that size shows it, wake only for what readNodes reads of a Node.
func (c *controller) run(ctx context.Context) {
	if c.watching {
		c.wake, c.ctx = make(chan struct{}, 1), ctx
		go c.api.WatchServices(ctx, c.changed("services"))
		go c.api.WatchNodes(ctx, c.changed("nodes"))
	}
	for {
		err := c.turn(ctx)
		if err != nil && ctx.Err() == nil {
			c.log.Warn("lb-controller: turn", "err", err)
		}
		wait := c.interval
		if c.watching && err == nil && !c.open {
			wait = max(time.Until(c.lastSent.Add(c.resync)), 0)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-c.wake:
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.debounce):
			}
			select { // what came meanwhile is in the turn that starts now
			case <-c.wake:
			default:
			}
		}
	}
}

// changed is what one watch calls: it asks for a turn. A watch that fails is said once per failure, not at each of
// its attempts.
func (c *controller) changed(what string) func(error) {
	last := ""
	return func(err error) {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != "" && msg != last {
			c.log.Warn("lb-controller: watch", "of", what, "err", msg)
		}
		last = msg
		select {
		case c.wake <- struct{}{}:
		default: // a turn is asked for already
		}
	}
}

// follow keeps a watch on each Secret the Services of this turn name, and on no other: one watch per name, in the
// Service's namespace, ended when no Service names the Secret any more. A cluster's balancer holds at most
// lbgw.MaxClusterCerts certificates, so no more Secrets than that are watched; one beyond is read at the resync.
func (c *controller) follow(req *lbgw.Request) {
	if !c.watching {
		return
	}
	named := map[string]bool{}
	for _, s := range req.Services {
		for _, ref := range s.Certificates {
			if ref.Secret != "" {
				named[s.Namespace+"/"+ref.Secret] = true
			}
		}
	}
	for key, stop := range c.secrets {
		if !named[key] {
			stop()
			delete(c.secrets, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(named)) {
		if c.secrets[key] != nil || len(c.secrets) >= lbgw.MaxClusterCerts {
			continue
		}
		if c.secrets == nil {
			c.secrets = map[string]context.CancelFunc{}
		}
		ctx, stop := context.WithCancel(c.ctx)
		c.secrets[key] = stop
		ns, name, _ := strings.Cut(key, "/")
		go c.api.WatchSecret(ctx, ns, name, c.changed("secret "+key))
	}
}

// gateway is the portal's side. The token and the CA are read again for each call, so a rotation (the operator's
// "Enable load balancer controller" again) reaches a running controller when the kubelet updates the Secret volume.
type gateway struct {
	url, dir string
}

// protocol1 is req without what protocol 2 added, and without the Services a portal of protocol 1 would serve as
// they must never be served. It reads no annotation, so it would give a Service that asks for an internal balancer
// a public address, and it would hand the port of a Service that asks for TLS to its members as it comes, without
// TLS: what the members answer in the clear would be public. No certificate is sent to it either.
func protocol1(req *lbgw.Request) *lbgw.Request {
	out := &lbgw.Request{Services: []lbgw.Service{}, Nodes: req.Nodes}
	for _, s := range req.Services {
		if lbgw.Internal(s.Annotations) || lbgw.WantsTLS(&s) {
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
	// GetSecret is the controller's one read of a Secret: one name in one namespace (certificates).
	GetSecret(ctx context.Context, ns, name string) (typ string, data map[string][]byte, found bool, err error)
	// The watches, each until ctx ends: changed is called when something changed, and with the error when a
	// stream failed. WatchSecret is of one name in one namespace (follow): there is no watch of all Secrets.
	WatchServices(ctx context.Context, changed func(error))
	WatchNodes(ctx context.Context, changed func(error))
	WatchSecret(ctx context.Context, ns, name string, changed func(error))
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
	// sent is when the Secret of each hash was last sent to the portal (turn).
	// ponytail: per process; a controller that starts may send a Secret the portal asks for once more.
	sent map[string]time.Time

	// When a turn comes (run). The zero values are a controller that sends at every turn.
	watching                   bool
	interval, resync, debounce time.Duration
	ctx                        context.Context               // run's, for the watches
	wake                       chan struct{}                 // a watch asks for a turn
	secrets                    map[string]context.CancelFunc // the watches of Secrets, by namespace/name (follow)
	// lastReq is the state the portal last answered, as it was sent without certificate data, lastAns its answer
	// and lastSent when: an equal state is not sent again before resync, the answer stands for it (turn).
	lastReq  []byte
	lastAns  *lbgw.Answer
	lastSent time.Time
	// open: the last turn left something for the next one, which is therefore sent whatever its state.
	open bool
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
	wasOpen := c.open
	c.open = false
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
	hosts := map[string]string{}                 // by UID: the addresses behind the Service this turn
	secrets := map[string]lbgw.CertificateData{} // by hash: the Secrets the Services of this turn name, for this turn
	var owned []*service
	for _, s := range services {
		if !s.owned || s.deleting {
			continue
		}
		if !s.held() {
			// Without the finalizer a delete would not wait for the portal: such a Service is sent from the next turn.
			if err := c.setFinalizers(ctx, s, append(slices.Clone(s.finalizers), Finalizer)); err != nil {
				c.log.Warn("lb-controller: add finalizer", "service", s.ns+"/"+s.name, "err", err)
				c.open = true
				continue
			}
		}
		behind := addressesOf(nodes)
		if sel := parseSelector(s.nodeSelector); len(sel) > 0 {
			behind = slices.DeleteFunc(behind, func(a string) bool { return !selects(sel, labels[a]) })
			s.wire.Nodes = &behind
		}
		hosts[s.uid] = strings.Join(behind, ",")
		s.wire.Certificates = c.certificates(ctx, s, secrets)
		req.Services = append(req.Services, s.wire)
		owned = append(owned, s)
	}
	c.follow(req)
	// A state equal to the one the portal last answered is not sent again before the resync: a Node's heartbeat
	// is a list and no request. The last answer then stands for it, and the rest of the turn runs as after a send,
	// so a status or an Event that could not be written is written now. A turn the last one left open is sent.
	state, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ans := c.lastAns
	if ans == nil || wasOpen || !bytes.Equal(state, c.lastReq) || time.Since(c.lastSent) >= c.resync {
		c.lastAns, c.lastSent = nil, time.Now()
		ans, err = c.put(ctx, req)
	}
	if err == nil && len(ans.NeedCertificates) > 0 {
		// The portal holds no certificate for these hashes: a new Secret, or one that changed. Their Secrets are
		// sent now, once, and only the ones a Service of this turn names: the portal cannot ask for another.
		if c.sent == nil {
			c.sent = map[string]time.Time{}
		}
		for _, hash := range ans.NeedCertificates {
			// Once, and not again within sentEvery whatever the portal answers: a portal that cannot keep what
			// it was sent does not get a key at every turn.
			if data, ok := secrets[hash]; ok && time.Since(c.sent[hash]) >= sentEvery {
				req.CertificateData, c.sent[hash] = append(req.CertificateData, data), time.Now()
			}
		}
		for hash := range c.sent { // a Secret that changed or went: its hash is not asked for again
			if _, ok := secrets[hash]; !ok {
				delete(c.sent, hash)
			}
		}
		if len(req.CertificateData) > 0 {
			c.lastAns = nil
			ans, err = c.put(ctx, req)
		}
	}
	if err == nil {
		c.lastReq, c.lastAns = state, ans
		c.open = c.open || len(ans.NeedCertificates) > 0
	}
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
			// Not sent: an internal Service, or one that asks for TLS, and a portal of protocol 1 (protocol1). It has
			// no address there.
			switch {
			case lbgw.Internal(s.wire.Annotations):
				if len(s.ingress) > 0 {
					c.setStatus(ctx, s, "")
				}
				c.report(ctx, s, "state", "Warning", lbgw.ReasonInternalNotSupported, lbgw.InternalMessage)
			case lbgw.WantsTLS(&s.wire):
				if len(s.ingress) > 0 {
					c.setStatus(ctx, s, "")
				}
				c.report(ctx, s, "state", "Warning", lbgw.ReasonTLSNotAvailable, "The portal is older than this cluster's load balancer controller and ends no TLS: the Service is not served, so that its port is never public without TLS, until the portal is upgraded")
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
		if st.IPMode != "" { // protocol 2: the portal read the annotations
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
			c.open = true // the portal says ready in an answer to come
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
			c.open = true
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

// certificates reads the Secrets Service s names in proxcloud.io/lb-tls-secret, and answers each one's name and the
// hash of its content. What it read goes into secrets by that hash, for this turn only: it is sent when the portal
// asks for the hash, and dropped with the turn.
//
// What the controller may read of the tenant's Secrets is decided here, since its kubeconfig could read them all:
// only a Secret an owned Service names, by a GET of that name (it never lists Secrets, and the one watch it has is
// of that name and reads nothing: follow), only in the
// Service's own namespace (a name with a namespace in it is no name: lbgw.TLSSecretNames), and only one of type
// kubernetes.io/tls: a Secret of another type is not hashed, not kept and not sent. Nothing of a Secret is logged.
//
// A Secret that cannot be used is answered with the reason. The portal then leaves the Service as the balancer last
// served it, or refuses one it never served, and says InvalidCertificate on the Service: a deleted Secret takes no
// certificate off a balancer. A read the API server forbids (a controller given an identity without "get" on
// Secrets) is one such reason, and every other Service is served as before.
func (c *controller) certificates(ctx context.Context, s *service, secrets map[string]lbgw.CertificateData) []lbgw.CertRef {
	value, set := s.wire.Annotations[lbgw.OwnPrefix+lbgw.KnobTLSSecret]
	if value = strings.TrimSpace(value); !set || value == "" {
		return nil
	}
	names, ok := lbgw.TLSSecretNames(value)
	if !ok {
		return []lbgw.CertRef{{Problem: lbgw.CertBadNames}}
	}
	refs := make([]lbgw.CertRef, 0, len(names))
	for _, name := range names {
		ref := lbgw.CertRef{Secret: name}
		typ, data, found, err := c.api.GetSecret(ctx, s.ns, name)
		var ae *kamajik8s.APIError
		switch crt, key := data["tls.crt"], data["tls.key"]; {
		case errors.As(err, &ae) && ae.Status == http.StatusForbidden:
			ref.Problem = lbgw.CertForbidden
		case err != nil:
			c.log.Warn("lb-controller: read a TLS Secret", "service", s.ns+"/"+s.name, "secret", name, "err", err)
			ref.Problem = lbgw.CertUnreadable
		case !found:
			ref.Problem = lbgw.CertNotFound
		case typ != lbgw.TLSSecretType:
			ref.Problem = lbgw.CertWrongType
		case len(crt) == 0 || len(key) == 0:
			ref.Problem = lbgw.CertIncomplete
		case len(crt)+len(key) > lbgw.MaxCertBundle:
			ref.Problem = lbgw.CertTooLarge
		case !utf8.Valid(crt) || !utf8.Valid(key):
			// PEM is text. Bytes that are none would not cross to the portal as they are, so the portal could
			// never hold what this hash names and would ask for it at every turn.
			ref.Problem = lbgw.CertNotText
		default:
			ref.SHA256 = lbgw.SecretHash(crt, key)
			secrets[ref.SHA256] = lbgw.CertificateData{Certificate: string(crt), PrivateKey: string(key)}
		}
		refs = append(refs, ref)
	}
	return refs
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
			// over the gateway's body limit. The spaces around it go first, as the portal's readers drop them: the
			// cut must not take the "true" off an internal annotation.
			val := str(v)
			if len(val) > lbgw.MaxAnnotationValue {
				if val = strings.TrimSpace(val); len(val) > lbgw.MaxAnnotationValue {
					val = val[:lbgw.MaxAnnotationValue+1]
				}
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
		c.open = true
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
