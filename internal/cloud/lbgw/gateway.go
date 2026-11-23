// Package lbgw is the load balancer gateway: the one call a tenant cluster's load balancer controller makes, PUT
// /lb/v1/state, on the gateway listener next to the CSI gateway (cmd/api routes /lb/v1/ here).
//
// The controller presents a per-cluster credential (lb@proxcloud!<clusterId>) and sends the cluster's whole desired
// state: its Services of type LoadBalancer and its nodes' addresses. Nothing in the request names a project, a network
// or a VM: those come from the credential row. The gateway checks each Service on its own (TCP, UDP and IPv4 only, a
// node port in range, source ranges that fit a firewall rule, annotations it can honour: annotations.go) and hands the
// rest to the portal (Portal), which stores the
// state and converges the cluster's balancer in the background: the answer is the state as it is, never a build
// inside the request. See docs/managed-k8s.md and the v0.11.0 load balancer plan.
package lbgw

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
//
// Protocol 2 (v0.11.1) only adds: a Service's annotations, session affinity, own nodes, certificates and the address
// it asks for (loadBalancerIP) in the request, and the certificates the portal asked for; its ipMode and Events in the answer, and the certificates the
// portal asks for (tls.go). A protocol 1 controller sends none of them and is served as before, with one thing kept
// back: its UDP Services stay refused (ProtocolUDP), so that upgrading the portal alone never hands a Service that
// stood pending a public address, which is billed. The cluster's owner gets UDP by upgrading the cluster.
const (
	ProtocolHeader = "ProxCloud-LB-Protocol"
	Protocol       = 2
	MinProtocol    = 1
	// ProtocolUDP is the first protocol whose controllers get UDP ports served.
	ProtocolUDP = 2
	StatePath   = "/lb/v1/state"
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
	ReasonUDPNotSupported          = "UDPNotSupported"      // a UDP port from a controller older than ProtocolUDP: the whole Service (C14)
	ReasonProtocolNotSupported     = "ProtocolNotSupported" // a port that is neither TCP nor UDP (SCTP): the whole Service
	ReasonInternalNotSupported     = "InternalNotSupported" // InternalAnnotation: the balancer's addresses are public
	ReasonNoNodePort               = "NoNodePort"           // allocateLoadBalancerNodePorts: false, or a port out of range
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
	maxBody        = 64 << 10 // the state's part of a request; the certificates the portal asked for come on top (maxCertBody)
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
	// CertificateData (protocol 2) are the Secrets whose hashes the last answer asked for (Answer.NeedCertificates),
	// and no other.
	CertificateData []CertificateData `json:"certificateData,omitempty"`
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
	// ProxyProtocol: the annotation proxcloud.io/lb-proxy-protocol is "true", so members get a PROXY v2 header. It is
	// protocol 1's one knob; a protocol 2 controller still sends it, for a portal that reads no Annotations.
	ProxyProtocol bool     `json:"proxyProtocol,omitempty"`
	IPFamilies    []string `json:"ipFamilies,omitempty"`

	// Protocol 2, each optional.
	//
	// Annotations are the Service's annotations of the names SentAnnotation takes, as the tenant wrote them (a value
	// over MaxAnnotationValue may come cut: it is not read either way). Any other name is not read and not counted.
	Annotations map[string]string `json:"annotations,omitempty"`
	// SessionAffinity ClientIP keeps a client on one node for SessionAffinityTimeout seconds (0 = Kubernetes' three
	// hours).
	SessionAffinity        string `json:"sessionAffinity,omitempty"`
	SessionAffinityTimeout int    `json:"sessionAffinityTimeout,omitempty"`
	// Nodes, when set, are the only nodes behind this Service, by address: the ones of the request's Nodes that its
	// node selector annotation matches. An empty list is a selector that matches none. nil: every node.
	Nodes *[]string `json:"nodes,omitempty"`
	// Certificates are the Secrets the Service's proxcloud.io/lb-tls-secret names, in its order: each one's name
	// and the hash of its content, never the content (tls.go). The first one answers a client that asks for no name.
	Certificates []CertRef `json:"certificates,omitempty"`
	// LoadBalancerIP is the Service's spec.loadBalancerIP: the address it asks for. It is obeyed for an address its
	// own cluster keeps, when the Service gets its address, and for no other (ServiceOptions; the portal's
	// lbClusterDesired).
	LoadBalancerIP string `json:"loadBalancerIP,omitempty"`
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
	// NeedCertificates (protocol 2) are the hashes of the request's certificates the portal holds no certificate
	// for: the controller sends those Secrets, once, in its next request.
	NeedCertificates []string `json:"needCertificates,omitempty"`
}

// ServiceStatus is one Service's state. Address is the public address once it has one.
type ServiceStatus struct {
	UID     string `json:"uid"`
	Address string `json:"address,omitempty"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Protocol 2, for a Service that is not refused. IPMode is the ipMode of the address in the Service's status (VIP
	// or Proxy). Events are what the Service's annotations raised, for the controller to say on the Service.
	IPMode string  `json:"ipMode,omitempty"`
	Events []Event `json:"events,omitempty"`
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
	Seen(ctx context.Context, clusterID string, at time.Time, protocol int) error
}

// Portal is the project side: it filters the nodes against the cluster's workers, takes the certificates it asked
// for, stores the state and starts the converge, and answers each Service as the cluster's balancer has it now. It
// may refuse the whole request with a *Refusal (a project that is suspended or being deleted).
type Portal interface {
	LBClusterState(ctx context.Context, cred *Credential, services []Service, nodes []Node, certs []CertificateData) (*Answer, error)
}

// Handler serves /lb/v1/ on the gateway listener.
type Handler struct {
	Reg    Registry
	Portal Portal
	// Audit records refusals. nil = log only.
	Audit func(audit.AuditEvent)
	Log   *slog.Logger

	mu      sync.Mutex
	audited map[string]string    // cluster -> the refusals last audited
	denied  map[string]time.Time // known cluster and reason -> when a refused credential was last audited
	now     func() time.Time     // the tests' clock; nil = time.Now
}

// Why a credential was refused. The caller is told none of it: every refusal is the same 401.
const (
	denyMalformed   = "malformed"       // no header of the expected shape, or an id that is not a cluster id
	denyUnknown     = "unknown cluster" // no credential was ever minted for the cluster it names
	denyRevoked     = "revoked"         // the cluster was released, or its credential was minted again since
	denyWrongSecret = "wrong secret"
	deniedEvery     = time.Minute
)

// denial is a refused credential: the cluster it claimed ("" unless the header named one in the form of a cluster id,
// so nothing a caller made up reaches a log line), why, and the cluster's own credential row when it has one. That
// row says whose cluster the caller claimed to be; it authenticates nobody.
type denial struct {
	cluster string
	why     string
	known   *Credential
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
	cred, deny, err := h.authenticate(ctx, r.Header.Get("Authorization"))
	if err != nil {
		h.log().Error("lb gateway: credential lookup", "err", err)
		writeRefusal(w, &Refusal{http.StatusServiceUnavailable, "", "lb gateway: try again"})
		return
	}
	if deny != nil {
		h.log().Warn("lb gateway: refused credential", "remote", r.RemoteAddr, "cluster", deny.cluster, "reason", deny.why)
		h.auditDenied(deny, r.RemoteAddr)
		writeRefusal(w, &Refusal{http.StatusUnauthorized, "", "authentication failure"})
		return
	}
	// The protocol the controller names is kept with its last call, and written at once when it changes: a cluster
	// that was bumped to the portal's chart stops reading as an old controller with its first call. A header that is
	// no number is 0, which serve refuses below, and so is one that is out of range or negative: the row never holds
	// what strconv made of a header it could not read.
	named, err := strconv.Atoi(r.Header.Get(ProtocolHeader))
	if err != nil || named < 0 {
		named = 0
	}
	if cred.LastSeenAt == nil || time.Since(*cred.LastSeenAt) > seenEvery || cred.Protocol != named {
		if err := h.Reg.Seen(ctx, cred.ID, time.Now().UTC(), named); err != nil {
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
// from the credential alone. It returns the credential, or why there is none (a denial), or the lookup's error.
func (h *Handler) authenticate(ctx context.Context, header string) (*Credential, *denial, error) {
	tok, ok := strings.CutPrefix(header, "PVEAPIToken=")
	if !ok {
		tok, ok = strings.CutPrefix(header, "PVEAPIToken ")
	}
	if !ok {
		return nil, &denial{why: denyMalformed}, nil
	}
	id, secret, ok := strings.Cut(tok, "=")
	clusterID, isLB := strings.CutPrefix(id, TokenUser+"!")
	if !isLB || !clusterIDRe.MatchString(clusterID) {
		return nil, &denial{why: denyMalformed}, nil
	}
	if !ok || secret == "" {
		return nil, &denial{cluster: clusterID, why: denyMalformed}, nil
	}
	cred, err := h.Reg.Credential(ctx, clusterID)
	if err != nil {
		return nil, nil, err
	}
	// The hash is compared for an unknown cluster too, so the answer takes as long whether the id exists or not.
	want := noSecretHash
	if cred != nil {
		want = cred.SecretSHA256
	}
	match := subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(want)) == 1
	switch {
	case cred == nil:
		return nil, &denial{cluster: clusterID, why: denyUnknown}, nil
	case cred.RevokedAt != nil:
		return nil, &denial{cluster: clusterID, why: denyRevoked, known: cred}, nil
	case !match:
		return nil, &denial{cluster: clusterID, why: denyWrongSecret, known: cred}, nil
	}
	return cred, nil, nil
}

// auditDenied audits a credential refused for a cluster that has one: a revoked credential still in use (a controller
// that kept its old Secret after a rotation, or one that outlived its cluster) and a wrong secret (someone guessing,
// or another cluster's Secret). At most one event per cluster and reason in deniedEvery, since a controller comes
// back every ten seconds; the log line has every refusal. A header that names no known cluster is only logged: it
// has no project to belong to, and anyone who reaches the listener could fill the audit log with them. The secret
// is never part of either.
// ponytail: the last time is kept per process, for the clusters that have a credential (two reasons each).
func (h *Handler) auditDenied(d *denial, remote string) {
	if d.known == nil {
		return
	}
	now := time.Now()
	if h.now != nil {
		now = h.now()
	}
	key := d.cluster + " " + d.why
	h.mu.Lock()
	if h.denied == nil {
		h.denied = map[string]time.Time{}
	}
	last, seen := h.denied[key]
	due := !seen || now.Sub(last) >= deniedEvery
	if due {
		h.denied[key] = now
	}
	h.mu.Unlock()
	if due {
		h.audit(d.known, "credential refused: "+d.why, map[string]any{"clusterId": d.cluster, "status": http.StatusUnauthorized,
			"reason": d.why, "remote": remote, "authenticated": false})
	}
}

func (h *Handler) serve(ctx context.Context, w http.ResponseWriter, r *http.Request, cred *Credential) (*Answer, error) {
	if r.URL.Path != StatePath || r.URL.RawQuery != "" {
		return nil, &Refusal{http.StatusNotFound, "", "lb gateway: the one call is PUT " + StatePath}
	}
	if r.Method != http.MethodPut {
		return nil, &Refusal{http.StatusMethodNotAllowed, "", "lb gateway: the one call is PUT " + StatePath}
	}
	protocol, err := strconv.Atoi(r.Header.Get(ProtocolHeader))
	if err != nil || protocol < MinProtocol {
		return nil, &Refusal{http.StatusConflict, ReasonControllerTooOld,
			fmt.Sprintf("This cluster's load balancer controller is older than the portal accepts (protocol %d or later). Bump the cluster to the portal's chart", MinProtocol)}
	}
	// The body is the state and, when the last answer asked for them, certificates: the state's part keeps its
	// limit, and the certificates are measured as they were sent.
	var body struct {
		Request
		CertificateData json.RawMessage `json:"certificateData"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody+maxCertBody))
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(&body)
	}
	// Over the limit: a body that was cut, a state that is too large, and a body of that size without certificates
	// that is no state either.
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || (err == nil && len(raw)-len(body.CertificateData) > maxBody) ||
		(err != nil && len(raw) > maxBody && !bytes.Contains(raw, []byte("certificateData"))) {
		// Nothing was stored: the cluster's balancer stays as the last state that fitted made it.
		return nil, &Refusal{http.StatusRequestEntityTooLarge, ReasonTooLarge, fmt.Sprintf("The cluster's load balancer state is over %d KiB and was not taken: the balancer stays as it is, and no change to a Service or a node reaches it until the state fits. Shorten the Services' load balancer annotations and source ranges", maxBody>>10)}
	}
	req := body.Request
	if err == nil && len(body.CertificateData) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body.CertificateData))
		dec.DisallowUnknownFields()
		if dec.Decode(&req.CertificateData) != nil || len(req.CertificateData) > MaxClusterCerts {
			return nil, &Refusal{http.StatusBadRequest, ReasonInvalid, fmt.Sprintf("lb gateway: certificateData is at most %d certificates, each with its certificate and privateKey", MaxClusterCerts)}
		}
	}
	if err != nil {
		// A body that holds a key says nothing of itself in an error: a decoder names what it stumbled over.
		if bytes.Contains(raw, []byte("PRIVATE KEY")) || bytes.Contains(raw, []byte("certificateData")) {
			return nil, &Refusal{http.StatusBadRequest, ReasonInvalid, "lb gateway: the body is not a state"}
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
	options := make([]Options, len(req.Services))
	var accepted []Service
	at := map[string]int{}
	named := map[string]bool{} // the certificates of the Services accepted so far, by hash
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
		var reason, msg string
		if options[i], reason, msg = CheckService(s, i, protocol); reason != "" {
			statuses[i].State, statuses[i].Reason, statuses[i].Message = StateRefused, reason, msg
			continue
		}
		if protocol < 2 { // no controller of protocol 1 sends them, and they are read for none
			s.Certificates, s.LoadBalancerIP = nil, ""
		}
		more := 0
		for j, c := range s.Certificates {
			if c.SHA256 != "" && !named[c.SHA256] && !slices.ContainsFunc(s.Certificates[:j], func(b CertRef) bool { return b.SHA256 == c.SHA256 }) {
				more++
			}
		}
		if len(named)+more > MaxClusterCerts {
			statuses[i].State, statuses[i].Reason = StateRefused, ReasonTooLarge
			statuses[i].Message = fmt.Sprintf("A cluster's load balancer holds at most %d certificates: name fewer TLS Secrets in the cluster's Services", MaxClusterCerts)
			continue
		}
		for _, c := range s.Certificates {
			if c.SHA256 != "" {
				named[c.SHA256] = true
			}
		}
		accepted = append(accepted, *s)
	}
	ans, err := h.Portal.LBClusterState(ctx, cred, accepted, req.Nodes, req.CertificateData)
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
		if statuses[i].State != StateRefused && protocol >= 2 {
			// The portal's own Events (a certificate that cannot be used) come before the annotations'.
			statuses[i].IPMode, statuses[i].Events = options[i].IPMode(&req.Services[i]), append(statuses[i].Events, options[i].Events...)
		}
	}
	ans.Services = statuses
	return ans, nil
}

// CheckService refuses a Service the balancer cannot serve, puts its source ranges in the form a firewall rule takes
// and reads its annotations (ServiceOptions). index is its place in the request: the ones past MaxServices are
// refused. protocol is the controller's: one older than ProtocolUDP gets no UDP port served.
func CheckService(s *Service, index, protocol int) (o Options, reason, message string) {
	if reason, message = checkService(s, index, protocol); reason != "" {
		return o, reason, message
	}
	return ServiceOptions(s)
}

func checkService(s *Service, index, protocol int) (reason, message string) {
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
	if reason, message = checkCertRefs(s.Certificates); reason != "" {
		return reason, message
	}
	if s.Nodes != nil {
		if len(*s.Nodes) > MaxNodes {
			return ReasonInvalid, fmt.Sprintf("A Service takes at most %d nodes", MaxNodes)
		}
		for _, n := range *s.Nodes {
			if a, err := netip.ParseAddr(n); err != nil || !a.Is4() {
				return ReasonInvalid, "A Service's nodes are named by their IPv4 InternalIP"
			}
		}
	}
	seen := map[string]bool{} // a port is (protocol, port): TCP 53 and UDP 53 are two listeners
	for i := range s.Ports {
		p := &s.Ports[i]
		if p.Protocol == "" {
			p.Protocol = "TCP" // as in Kubernetes
		}
		switch {
		case p.Protocol == "UDP" && protocol < ProtocolUDP:
			// Not served for a controller of before v0.11.1, whatever the portal can do: the Service would get a
			// public address, which is billed, without anyone having asked for it since the portal's upgrade.
			return ReasonUDPNotSupported, "This cluster's load balancer controller serves TCP only, and port " + strconv.Itoa(p.Port) + " is UDP: no port of this Service is served. For UDP, upgrade the cluster"
		case p.Protocol != "TCP" && p.Protocol != "UDP":
			return ReasonProtocolNotSupported, "The load balancer serves TCP and UDP, and port " + strconv.Itoa(p.Port) + " is " + p.Protocol + ": no port of this Service is served"
		}
		key := p.Protocol + "/" + strconv.Itoa(p.Port)
		if p.Port < 1 || p.Port > 65535 || seen[key] {
			return ReasonInvalid, "Port " + strconv.Itoa(p.Port) + " is out of range or listed twice"
		}
		seen[key] = true
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
