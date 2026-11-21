// Package lbgw is the load balancer gateway: the one call a tenant cluster's load balancer controller makes, PUT
// /lb/v1/state, on the gateway listener next to the CSI gateway (cmd/api routes /lb/v1/ here).
//
// The controller presents a per-cluster credential (lb@proxcloud!<clusterId>) and sends the cluster's whole desired
// state: its Services of type LoadBalancer and its nodes' addresses. Nothing in the request names a project, a network
// or a VM: those come from the credential row. The gateway checks each Service on its own (TCP and IPv4 only, a node
// port in range, source ranges that fit a firewall rule) and hands the rest to the portal (Portal), which stores the
// state and converges the cluster's balancer in the background: the answer is the state as it is, never a build
// inside the request. See docs/managed-k8s.md and the v0.11.0 load balancer plan.
package lbgw

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

// The protocol of /lb/v1. A controller sends its own in ProtocolHeader; the gateway answers ControllerTooOld to one
// older than MinProtocol, so a change that needs a newer controller says "bump the cluster" instead of failing
// silently. /lb/v1 stays backward compatible within a minor release (C12).
const (
	ProtocolHeader = "ProxCloud-LB-Protocol"
	Protocol       = 1
	MinProtocol    = 1
	StatePath      = "/lb/v1/state"
)

// What a Service is answered with.
const (
	StateReady    = "ready"
	StateBuilding = "building"
	StateRefused  = "refused"
)

// Reasons, which the controller turns into Events.
const (
	ReasonQuotaExceeded            = "QuotaExceeded"
	ReasonUDPNotSupported          = "UDPNotSupported" // any port that is not TCP: the whole Service (C14)
	ReasonNoNodePort               = "NoNodePort"      // allocateLoadBalancerNodePorts: false, or a port out of range
	ReasonIPFamilyNotSupported     = "IPFamilyNotSupported"
	ReasonTooManySourceRanges      = "TooManySourceRanges"
	ReasonPublicNetworkUnavailable = "PublicNetworkUnavailable"
	ReasonTooLarge                 = "TooLarge"
	ReasonInvalid                  = "Invalid"
	ReasonNotAWorker               = "NotAWorker"
	ReasonControllerTooOld         = "ControllerTooOld"
	ReasonProjectNotEnabled        = "ProjectNotEnabled"
)

// Limits of one state. The 45 KiB config cap (pve.LBMaxConfig) is the portal's check, on the rendered config.
const (
	MaxServices    = 50
	MaxPorts       = 20
	MaxNodes       = 64
	maxBody        = 64 << 10
	NodePortMin    = 30000
	NodePortMax    = 32767
	seenEvery      = time.Minute
	maxNameLen     = 253
	maxSourceRange = 100
)

// Request is the body of PUT /lb/v1/state.
type Request struct {
	Services []Service `json:"services"`
	Nodes    []Node    `json:"nodes"`
}

// Service is one Service of type LoadBalancer the controller owns. Namespace and Name are for people: they never reach
// a config.
type Service struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ports     []Port `json:"ports"`
	// ExternalTrafficPolicy Local is checked on HealthCheckNodePort, which kube-proxy answers 200 only on a node with an
	// endpoint of the Service.
	ExternalTrafficPolicy string   `json:"externalTrafficPolicy,omitempty"`
	HealthCheckNodePort   int      `json:"healthCheckNodePort,omitempty"`
	SourceRanges          []string `json:"sourceRanges,omitempty"`
	// ProxyProtocol: the annotation proxcloud.io/lb-proxy-protocol is "true", so members get a PROXY v2 header.
	ProxyProtocol bool     `json:"proxyProtocol,omitempty"`
	IPFamilies    []string `json:"ipFamilies,omitempty"`
}

// Port is one port of a Service.
type Port struct {
	Name     string `json:"name,omitempty"`
	Protocol string `json:"protocol,omitempty"` // TCP when empty, as in Kubernetes
	Port     int    `json:"port"`
	NodePort int    `json:"nodePort"`
}

// Node is one node the controller would put behind the balancer: its InternalIP.
type Node struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// Answer is the reply to a PUT: every Service of the request, in its order, and the nodes that were refused.
type Answer struct {
	Services []ServiceStatus `json:"services"`
	Nodes    []NodeStatus    `json:"nodes,omitempty"`
}

// ServiceStatus is one Service's state. Address is the public address once it has one.
type ServiceStatus struct {
	UID     string `json:"uid"`
	Address string `json:"address,omitempty"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// NodeStatus is a refused node.
type NodeStatus struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// Refusal is an answer that changes nothing: the request as a whole is refused.
type Refusal struct {
	Status  int
	Reason  string
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// Registry is the credential side of the Store; the tests use one in memory.
type Registry interface {
	Credential(ctx context.Context, clusterID string) (*Credential, error)
	Seen(ctx context.Context, clusterID string, at time.Time) error
}

// Portal is the project side: it filters the nodes against the cluster's workers, stores the state and starts the
// converge, and answers each Service as the cluster's balancer has it now. It may refuse the whole request with a
// *Refusal (a project that is suspended or being deleted).
type Portal interface {
	LBClusterState(ctx context.Context, cred *Credential, services []Service, nodes []Node) (*Answer, error)
}

// Handler serves /lb/v1/ on the gateway listener.
type Handler struct {
	Reg    Registry
	Portal Portal
	// Audit records refusals. nil = log only.
	Audit func(audit.AuditEvent)
	Log   *slog.Logger

	mu      sync.Mutex
	audited map[string]string // cluster -> the refusals last audited
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

var (
	clusterIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	uidRe        = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	nameRe       = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	noSecretHash = hashSecret("")
)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set(ProtocolHeader, strconv.Itoa(Protocol))
	cred, err := h.authenticate(ctx, r.Header.Get("Authorization"))
	if err != nil {
		var rf *Refusal
		if errors.As(err, &rf) {
			h.log().Warn("lb gateway: refused credential", "remote", r.RemoteAddr)
			writeRefusal(w, rf)
			return
		}
		h.log().Error("lb gateway: credential lookup", "err", err)
		writeRefusal(w, &Refusal{http.StatusServiceUnavailable, "", "lb gateway: try again"})
		return
	}
	if cred.LastSeenAt == nil || time.Since(*cred.LastSeenAt) > seenEvery {
		if err := h.Reg.Seen(ctx, cred.ID, time.Now().UTC()); err != nil {
			h.log().Warn("lb gateway: last seen", "cluster", cred.ID, "err", err)
		}
	}
	ans, err := h.serve(ctx, w, r, cred)
	var rf *Refusal
	switch {
	case err == nil:
		h.auditAnswer(cred, ans)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ans)
	case errors.As(err, &rf):
		h.refused(cred, r.Method, r.URL.Path, rf)
		writeRefusal(w, rf)
	default:
		h.log().Error("lb gateway: state", "cluster", cred.ID, "err", err)
		writeRefusal(w, &Refusal{http.StatusServiceUnavailable, "", "lb gateway: try again"})
	}
}

func writeRefusal(w http.ResponseWriter, rf *Refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rf.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{"reason": rf.Reason, "message": rf.Message})
}

// authenticate reads `PVEAPIToken=lb@proxcloud!<clusterId>=<secret>`, the CSI gateway's header shape. The cluster comes
// from the credential alone.
func (h *Handler) authenticate(ctx context.Context, header string) (*Credential, error) {
	unauth := &Refusal{http.StatusUnauthorized, "", "authentication failure"}
	tok, ok := strings.CutPrefix(header, "PVEAPIToken=")
	if !ok {
		tok, ok = strings.CutPrefix(header, "PVEAPIToken ")
	}
	if !ok {
		return nil, unauth
	}
	id, secret, ok := strings.Cut(tok, "=")
	if !ok || secret == "" {
		return nil, unauth
	}
	clusterID, ok := strings.CutPrefix(id, TokenUser+"!")
	if !ok || !clusterIDRe.MatchString(clusterID) {
		return nil, unauth
	}
	cred, err := h.Reg.Credential(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	// The hash is compared for an unknown cluster too, so the answer takes as long whether the id exists or not.
	want := noSecretHash
	if cred != nil {
		want = cred.SecretSHA256
	}
	match := subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(want)) == 1
	if cred == nil || cred.RevokedAt != nil || !match {
		return nil, unauth
	}
	return cred, nil
}

func (h *Handler) serve(ctx context.Context, w http.ResponseWriter, r *http.Request, cred *Credential) (*Answer, error) {
	if r.URL.Path != StatePath || r.URL.RawQuery != "" {
		return nil, &Refusal{http.StatusNotFound, "", "lb gateway: the one call is PUT " + StatePath}
	}
	if r.Method != http.MethodPut {
		return nil, &Refusal{http.StatusMethodNotAllowed, "", "lb gateway: the one call is PUT " + StatePath}
	}
	if v, err := strconv.Atoi(r.Header.Get(ProtocolHeader)); err != nil || v < MinProtocol {
		return nil, &Refusal{http.StatusConflict, ReasonControllerTooOld,
			fmt.Sprintf("This cluster's load balancer controller is older than the portal accepts (protocol %d or later). Bump the cluster to the portal's chart", MinProtocol)}
	}
	var req Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, &Refusal{http.StatusRequestEntityTooLarge, ReasonTooLarge, fmt.Sprintf("lb gateway: the state takes at most %d KiB", maxBody>>10)}
		}
		return nil, &Refusal{http.StatusBadRequest, ReasonInvalid, "lb gateway: the body is not a state: " + err.Error()}
	}
	if len(req.Nodes) > MaxNodes {
		return nil, &Refusal{http.StatusBadRequest, ReasonInvalid, fmt.Sprintf("lb gateway: at most %d nodes", MaxNodes)}
	}
	for _, n := range req.Nodes {
		if a, err := netip.ParseAddr(n.Address); err != nil || !a.Is4() || len(n.Name) > maxNameLen {
			return nil, &Refusal{http.StatusBadRequest, ReasonInvalid, "lb gateway: a node needs its IPv4 InternalIP"}
		}
	}
	statuses := make([]ServiceStatus, len(req.Services))
	var accepted []Service
	at := map[string]int{}
	for i := range req.Services {
		s := &req.Services[i]
		if !uidRe.MatchString(s.UID) || len(s.Name) > maxNameLen || len(s.Namespace) > maxNameLen ||
			(s.Name != "" && !nameRe.MatchString(s.Name)) || (s.Namespace != "" && !nameRe.MatchString(s.Namespace)) {
			return nil, &Refusal{http.StatusBadRequest, ReasonInvalid, "lb gateway: a Service needs its uid, and Kubernetes names"}
		}
		if _, dup := at[s.UID]; dup {
			return nil, &Refusal{http.StatusBadRequest, ReasonInvalid, "lb gateway: Service " + s.UID + " is listed twice"}
		}
		at[s.UID] = i
		statuses[i] = ServiceStatus{UID: s.UID}
		if reason, msg := CheckService(s, i); reason != "" {
			statuses[i].State, statuses[i].Reason, statuses[i].Message = StateRefused, reason, msg
			continue
		}
		accepted = append(accepted, *s)
	}
	ans, err := h.Portal.LBClusterState(ctx, cred, accepted, req.Nodes)
	if err != nil {
		return nil, err
	}
	for _, st := range ans.Services {
		if i, ok := at[st.UID]; ok && statuses[i].State == "" {
			statuses[i] = st
		}
	}
	for i := range statuses {
		if statuses[i].State == "" { // the portal's answer has every Service it got; this is a guard
			statuses[i].State = StateBuilding
		}
	}
	ans.Services = statuses
	return ans, nil
}

// CheckService refuses a Service the balancer cannot serve, and puts its source ranges in the form a firewall rule
// takes. index is its place in the request: the ones past MaxServices are refused.
func CheckService(s *Service, index int) (reason, message string) {
	if index >= MaxServices {
		return ReasonTooLarge, fmt.Sprintf("A cluster's load balancer serves at most %d Services", MaxServices)
	}
	for _, f := range s.IPFamilies {
		if f != "IPv4" {
			return ReasonIPFamilyNotSupported, "The load balancer serves IPv4 Services only: set ipFamilies to [IPv4] (SingleStack)"
		}
	}
	if len(s.Ports) == 0 {
		return ReasonInvalid, "The Service has no port"
	}
	if len(s.Ports) > MaxPorts {
		return ReasonTooLarge, fmt.Sprintf("A Service takes at most %d ports on the load balancer", MaxPorts)
	}
	seen := map[int]bool{}
	for _, p := range s.Ports {
		if p.Protocol != "" && p.Protocol != "TCP" {
			return ReasonUDPNotSupported, "The load balancer serves TCP only, and port " + strconv.Itoa(p.Port) + " is " + p.Protocol + ": no port of this Service is served"
		}
		if p.Port < 1 || p.Port > 65535 || seen[p.Port] {
			return ReasonInvalid, "Port " + strconv.Itoa(p.Port) + " is out of range or listed twice"
		}
		seen[p.Port] = true
		if p.NodePort == 0 {
			return ReasonNoNodePort, "The load balancer sends traffic to node ports: set allocateLoadBalancerNodePorts to true"
		}
		if p.NodePort < NodePortMin || p.NodePort > NodePortMax {
			return ReasonNoNodePort, fmt.Sprintf("Node port %d is outside %d-%d, the range the workers' firewall opens", p.NodePort, NodePortMin, NodePortMax)
		}
	}
	switch s.ExternalTrafficPolicy {
	case "", "Cluster":
		s.ExternalTrafficPolicy, s.HealthCheckNodePort = "Cluster", 0
	case "Local":
		if s.HealthCheckNodePort < NodePortMin || s.HealthCheckNodePort > NodePortMax {
			return ReasonNoNodePort, fmt.Sprintf("externalTrafficPolicy Local needs a healthCheckNodePort in %d-%d", NodePortMin, NodePortMax)
		}
	default:
		return ReasonInvalid, "externalTrafficPolicy must be Cluster or Local"
	}
	if len(s.SourceRanges) > maxSourceRange {
		return ReasonTooManySourceRanges, fmt.Sprintf("At most %d source ranges", maxSourceRange)
	}
	var ranges []string
	for _, raw := range s.SourceRanges {
		r := strings.TrimSpace(raw)
		if !strings.Contains(r, "/") {
			r += "/32"
		}
		p, err := netip.ParsePrefix(r)
		switch {
		case err != nil:
			return ReasonInvalid, "Source range " + raw + " is not a network"
		case !p.Addr().Is4():
			return ReasonIPFamilyNotSupported, "The load balancer takes IPv4 source ranges only"
		case p.Bits() == 0: // 0.0.0.0/0 admits any: the same as no list
			s.SourceRanges = nil
			return "", ""
		}
		ranges = append(ranges, p.Masked().String())
	}
	out, msg := pve.LBSourceRanges(ranges)
	if msg != "" {
		return ReasonTooManySourceRanges, "The source ranges do not fit one firewall rule (512 characters): use fewer, wider networks"
	}
	s.SourceRanges = out
	return "", ""
}

// refused audits a request the gateway refused as a whole, once per change (auditChange).
func (h *Handler) refused(cred *Credential, method, path string, rf *Refusal) {
	h.log().Warn("lb gateway: refused", "cluster", cred.ID, "method", method, "path", path, "reason", rf.Reason, "message", rf.Message)
	h.auditChange(cred, fmt.Sprintf("request %d %s %s %s %s", rf.Status, rf.Reason, method, path, rf.Message), rf.Message,
		map[string]any{"clusterId": cred.ID, "method": method, "path": path, "status": rf.Status, "reason": rf.Reason})
}

// auditAnswer audits the Services and nodes an answer refused (auditChange).
func (h *Handler) auditAnswer(cred *Credential, ans *Answer) {
	var items []string
	for _, s := range ans.Services {
		if s.State == StateRefused {
			items = append(items, "service "+s.UID+" "+s.Reason)
		}
	}
	for _, n := range ans.Nodes {
		items = append(items, "node "+n.Address+" "+n.Reason)
	}
	slices.Sort(items)
	key := strings.Join(items, ",")
	h.auditChange(cred, key, "refused: "+key, map[string]any{"clusterId": cred.ID, "refused": items})
}

// auditChange audits a refusal when it differs from the cluster's last one: the controller sends its state every few
// seconds, and the same refusal is one event, not one a poll. An empty key (nothing refused) is not audited.
// ponytail: the last key is kept per process; a restart audits a standing refusal once more.
func (h *Handler) auditChange(cred *Credential, key, msg string, meta map[string]any) {
	h.mu.Lock()
	if h.audited == nil {
		h.audited = map[string]string{}
	}
	changed := h.audited[cred.ID] != key
	h.audited[cred.ID] = key
	h.mu.Unlock()
	if changed && key != "" {
		h.audit(cred, msg, meta)
	}
}

func (h *Handler) audit(cred *Credential, msg string, meta map[string]any) {
	if h.Audit == nil {
		return
	}
	ev := audit.SystemEvent()
	ev.RequestInterface = audit.InterfaceLBGateway
	ev.EventContext = audit.ContextProject
	ev.Actor = &audit.AuditActor{Type: audit.ActorSystem, ID: TokenID(cred.ID), DisplayName: "load balancer controller " + cred.ID}
	ev.Action, ev.Outcome = audit.ActionDeny, audit.OutcomeFailure
	ev.ResourceType, ev.ResourceID = audit.ResourceLoadBalancer, cred.ID
	ev.ProjectID, ev.ErrorMessage = cred.ProjectID, msg
	ev.ResourceMetadata = meta
	h.Audit(ev)
}

// Split serves /lb/v1/ with lb and every other path with rest, as it is: the CSI gateway keeps its own path checks.
func Split(lb, rest http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/lb/v1/") {
			lb.ServeHTTP(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	})
}
