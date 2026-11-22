package lbgw

// annotations.go: what a Service's annotations and session affinity ask of its listeners and pools. The table is read
// here, in the portal, and not in the controller: the controller sends the annotations of the two prefixes, and no
// other, as they are (protocol 2), so a new knob ships with the portal and needs no new controller in every cluster (C12). The one
// the controller reads itself is the node selector, which needs the nodes' labels (Annotation).
//
// The own name is proxcloud.io/lb-<knob>. A loadbalancer.openstack.org/<alias> annotation is obeyed where it means the
// same thing here as in OCCM, the cloud controller manager those names belong to (v1.37.1, loadbalancer.go:61-107
// and its guide to Services of type LoadBalancer; the repository and every line this file follows are named in
// docs/architecture/c4/CHANGELOG.md, 2026-11-23), so a manifest moved from a cluster that runs OCCM behaves the same
// without an edit. When both names are set the own one wins. Every other annotation of the two prefixes is named in
// one Warning Event (AnnotationNotSupported): nothing is ignored in silence.

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	OwnPrefix   = "proxcloud.io/lb-"
	AliasPrefix = "loadbalancer.openstack.org/"
	// InternalAnnotation asks OCCM for a balancer without a floating IP. Here the pair's addresses are public, so a
	// Service that sets it to true is refused and never gets one (ReasonInternalNotSupported).
	InternalAnnotation = "service.beta.kubernetes.io/openstack-internal-load-balancer"

	// MaxAnnotationValue: an annotation with a longer value is not read (Read). How many annotations a Service has
	// is no limit of its own: the table looks up the names it knows and names the others, and the size of the whole
	// request is the gateway's limit (maxBody).
	MaxAnnotationValue = 256
	// maxAnnotationName: the longest name Kubernetes takes (253 for the prefix, "/", 63 for the name).
	maxAnnotationName = 317
	// maxNamed: an Event names this many annotations and counts the rest.
	maxNamed = 32

	// Kubernetes' own default for sessionAffinityConfig.clientIP.timeoutSeconds, and its largest value.
	affinityDefault, affinityMax = 10800, 86400
	// A listener's connection limit goes up to the appliance's own budget (pve lbMaxConnections).
	maxConnectionLimit = 20000
	// A timeout is one second to a day, in milliseconds (pve lbMinTimeout, lbMaxTimeout).
	minTimeout, maxTimeout = 1000, 24 * 3600 * 1000
)

// Reasons of the Events a Service's annotations raise.
const (
	ReasonAnnotationNotSupported = "AnnotationNotSupported"
	ReasonInvalidAnnotation      = "InvalidAnnotation"
	// OCCM's name for a refused lb-method (events.go:26).
	ReasonLbMethodUnknown = "LoadBalancerLbMethodUnknown"
	// ReasonKeepAddressNotServed: lb-keep-address is read and not acted on yet.
	ReasonKeepAddressNotServed = "KeepAddressNotServed"
)

// The knobs, by their name after proxcloud.io/lb-.
const (
	KnobProxyProtocol        = "proxy-protocol"
	KnobMethod               = "method"
	KnobConnectionLimit      = "connection-limit"
	KnobTimeoutClientData    = "timeout-client-data"
	KnobTimeoutMemberConnect = "timeout-member-connect"
	KnobTimeoutMemberData    = "timeout-member-data"
	KnobXForwardedFor        = "x-forwarded-for"
	KnobEnableHealthMonitor  = "enable-health-monitor"
	KnobHealthDelay          = "health-monitor-delay"
	KnobHealthTimeout        = "health-monitor-timeout"
	KnobHealthMaxRetries     = "health-monitor-max-retries"
	KnobHealthMaxRetriesDown = "health-monitor-max-retries-down"
	KnobNodeSelector         = "node-selector"
	KnobKeepAddress          = "keep-address"
)

// aliases: knob -> the name after loadbalancer.openstack.org/ that means the same.
var aliases = map[string]string{
	KnobProxyProtocol: "proxy-protocol", KnobMethod: "lb-method", KnobConnectionLimit: "connection-limit",
	KnobTimeoutClientData: "timeout-client-data", KnobTimeoutMemberConnect: "timeout-member-connect",
	KnobTimeoutMemberData: "timeout-member-data", KnobXForwardedFor: "x-forwarded-for",
	KnobEnableHealthMonitor: "enable-health-monitor", KnobHealthDelay: "health-monitor-delay",
	KnobHealthTimeout: "health-monitor-timeout", KnobHealthMaxRetries: "health-monitor-max-retries",
	KnobHealthMaxRetriesDown: "health-monitor-max-retries-down", KnobNodeSelector: "node-selector",
	KnobKeepAddress: "keep-floatingip",
}

// methods: both spellings of an algorithm, under either name.
var methods = map[string]string{"roundrobin": "roundrobin", "leastconn": "leastconn", "source": "source",
	"round_robin": "roundrobin", "least_connections": "leastconn", "source_ip": "source"}

// SentAnnotation: the controller sends a Service's annotation of this name.
func SentAnnotation(key string) bool {
	return strings.HasPrefix(key, OwnPrefix) || strings.HasPrefix(key, AliasPrefix) || key == InternalAnnotation
}

// Internal: the Service asks for a balancer without a public address (InternalAnnotation is a true boolean). The
// gateway refuses it; the controller keeps it out of a request to a portal that reads no annotations.
func Internal(ann map[string]string) bool {
	on, err := strconv.ParseBool(strings.TrimSpace(ann[InternalAnnotation]))
	return err == nil && on
}

// Read returns the annotations that are read at all: the load balancer's names (SentAnnotation) whose value has at
// most MaxAnnotationValue characters. long are the names left out for their value, in order. Anything else in ann is
// not the load balancer's business and counts for nothing. The controller reads its node selector through Read too,
// so both sides leave the same annotation out.
func Read(ann map[string]string) (read map[string]string, long []string) {
	read = map[string]string{}
	for k, v := range ann {
		switch {
		case !SentAnnotation(k) || len(k) > maxAnnotationName:
		case len(v) > MaxAnnotationValue:
			long = append(long, k)
		default:
			read[k] = v
		}
	}
	slices.Sort(long)
	return read, long
}

// named lists names, which the caller sorted: at most maxNamed of them, and how many more there are.
func named(names []string) string {
	if more := len(names) - maxNamed; more > 0 {
		return strings.Join(names[:maxNamed], ", ") + fmt.Sprintf(" and %d more", more)
	}
	return strings.Join(names, ", ")
}

// Annotation reads one knob: the own name, then OCCM's alias. name is the annotation that was read.
func Annotation(ann map[string]string, knob string) (value, name string, ok bool) {
	if v, ok := ann[OwnPrefix+knob]; ok {
		return strings.TrimSpace(v), OwnPrefix + knob, true
	}
	if a := aliases[knob]; a != "" {
		if v, ok := ann[AliasPrefix+a]; ok {
			return strings.TrimSpace(v), AliasPrefix + a, true
		}
	}
	return "", "", false
}

// Event is something the controller says on the Service, in the answer's order.
type Event struct {
	Type    string `json:"type"` // Normal | Warning
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// Options is what a Service asks of its listeners and pools beyond its ports. The zero value is a Service without an
// annotation: the balancer's own defaults.
type Options struct {
	// ProxyVersion: the PROXY header the TCP ports' members get, 1 or 2; 0 = none.
	ProxyVersion int
	// Method: roundrobin, leastconn or source; "" = roundrobin.
	Method string
	// ConnectionLimit of each listener, per member on a UDP port; 0 = none.
	ConnectionLimit int
	// Milliseconds, 0 = the balancer's own. TCP ports only.
	TimeoutClientData, TimeoutMemberConnect, TimeoutMemberData int
	// XForwardedFor makes the TCP ports HTTP listeners, which add the header.
	XForwardedFor bool
	// NoMonitor: no health check (never with externalTrafficPolicy Local).
	NoMonitor bool
	// The health check's figures, 0 = the balancer's own: seconds, seconds, checks, checks.
	HealthDelay, HealthTimeout, HealthRise, HealthFall int
	// AffinityTimeout over 0: a client stays on its member for that many seconds without a new connection.
	AffinityTimeout int
	// KeepAddress: the Service's address is to outlive it. Read here, served by a later change.
	KeepAddress bool
	// Events are what the Service's owner is told: annotations that are not obeyed, values that are not understood.
	Events []Event
}

// IPMode is the ipMode of the Service's address in its status: Proxy when its TCP members get a PROXY header, so
// that traffic from inside the cluster goes through the balancer and gets the header too (KEP-1860), VIP otherwise.
func (o Options) IPMode(s *Service) string {
	if o.ProxyVersion != 0 && slices.ContainsFunc(s.Ports, func(p Port) bool { return p.Protocol != "UDP" }) {
		return "Proxy"
	}
	return "VIP"
}

// ServiceOptions reads a Service's annotations and affinity. A reason refuses the Service, and of the annotations
// the internal one alone gives one. An annotation that cannot be read never does: a value that is too long (Read) or
// not understood is not obeyed and named in one Event (InvalidAnnotation), and the Service is served with the
// default, since refusing it would give its address back over a typing mistake, or over an annotation left on a
// Service that v0.11.0 served without reading it. The forwarded-for header asked for next to PROXY is not obeyed in
// the same way. The number of annotations refuses nothing either: the ones the table does not know are named
// (AnnotationNotSupported).
func ServiceOptions(s *Service) (o Options, reason, message string) {
	// Read before anything is left out: a Service that asks for an internal balancer never gets a public address.
	if Internal(s.Annotations) {
		return o, ReasonInternalNotSupported, "The load balancer's addresses are public: for an address inside the cluster's network set loadBalancerClass proxcloud.io/internal and remove " + InternalAnnotation
	}
	ann, long := Read(s.Annotations)
	var invalid []string
	if len(long) > 0 {
		invalid = append(invalid, fmt.Sprintf("%s: longer than %d characters", named(long), MaxAnnotationValue))
	}
	bad := func(name, value, want string) {
		invalid = append(invalid, fmt.Sprintf("%s: %q is not %s", name, value, want))
	}
	number := func(knob string, lo, hi int, want string) int {
		v, name, ok := Annotation(ann, knob)
		if !ok {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			bad(name, v, want)
			return 0
		}
		return n
	}
	flag := func(knob string) (on, set bool) {
		v, name, ok := Annotation(ann, knob)
		if !ok {
			return false, false
		}
		on, err := strconv.ParseBool(v)
		if err != nil {
			bad(name, v, "true or false")
			return false, false
		}
		return on, true
	}

	if s.ProxyProtocol { // protocol 1: the controller read proxcloud.io/lb-proxy-protocol "true" itself
		o.ProxyVersion = 2
	}
	if v, name, ok := Annotation(ann, KnobProxyProtocol); ok {
		// The own name's "true" is version 2, as it shipped in v0.11.0; OCCM's "true" is version 1
		// (loadbalancer.go:531-542).
		own := strings.HasPrefix(name, OwnPrefix)
		switch strings.ToLower(v) {
		case "v1":
			o.ProxyVersion = 1
		case "v2":
			o.ProxyVersion = 2
		case "true":
			o.ProxyVersion = 1
			if own {
				o.ProxyVersion = 2
			}
		case "false", "":
			o.ProxyVersion = 0
		default:
			o.ProxyVersion = 0
			bad(name, v, "true, v1, v2 or false")
		}
	}
	if v, name, ok := Annotation(ann, KnobMethod); ok {
		if o.Method = methods[strings.ToLower(v)]; o.Method == "" {
			o.Events = append(o.Events, Event{Type: "Warning", Reason: ReasonLbMethodUnknown, Message: fmt.Sprintf(
				"%s: %q is not roundrobin, leastconn or source (ROUND_ROBIN, LEAST_CONNECTIONS, SOURCE_IP): the Service is balanced round robin", name, v)})
		}
	}
	if v, _, ok := Annotation(ann, KnobConnectionLimit); !ok || v != "-1" { // -1 is OCCM's "no limit"
		o.ConnectionLimit = number(KnobConnectionLimit, 1, maxConnectionLimit, fmt.Sprintf("-1 (no limit) or 1 to %d", maxConnectionLimit))
	}
	const ms = "1000 to 86400000 milliseconds (a second to a day)"
	o.TimeoutClientData = number(KnobTimeoutClientData, minTimeout, maxTimeout, ms)
	o.TimeoutMemberConnect = number(KnobTimeoutMemberConnect, minTimeout, maxTimeout, ms)
	o.TimeoutMemberData = number(KnobTimeoutMemberData, minTimeout, maxTimeout, ms)
	// The forwarded-for header and PROXY both carry the client's address, and OCCM takes a Service that asks for both
	// as an error (loadbalancer.go:1627-1631): its sync fails and a balancer that exists stays as it was, address
	// and all. A refusal here would give the address back instead, also of a Service that v0.11.0 served with its
	// own PROXY annotation and the other name left on it. So PROXY holds, and the header is not added.
	if on, _ := flag(KnobXForwardedFor); on && o.ProxyVersion != 0 {
		_, name, _ := Annotation(ann, KnobXForwardedFor)
		invalid = append(invalid, name+": the forwarded-for header is not added next to the PROXY header, which carries the client's address already; set one of the two")
	} else {
		o.XForwardedFor = on
	}
	if on, set := flag(KnobEnableHealthMonitor); set && !on {
		if s.ExternalTrafficPolicy == "Local" {
			// "A health monitor is required for services with externalTrafficPolicy: Local" (OCCM's guide).
			invalid = append(invalid, "the health monitor stays on: externalTrafficPolicy Local needs it to find the nodes that hold a pod")
		} else {
			o.NoMonitor = true
		}
	}
	o.HealthDelay = number(KnobHealthDelay, 1, 300, "1 to 300 seconds")
	o.HealthTimeout = number(KnobHealthTimeout, 1, 300, "1 to 300 seconds")
	o.HealthRise = number(KnobHealthMaxRetries, 1, 10, "1 to 10 checks")
	o.HealthFall = number(KnobHealthMaxRetriesDown, 1, 10, "1 to 10 checks")
	if o.KeepAddress, _ = flag(KnobKeepAddress); o.KeepAddress {
		_, name, _ := Annotation(ann, KnobKeepAddress)
		o.Events = append(o.Events, Event{Type: "Warning", Reason: ReasonKeepAddressNotServed,
			Message: name + " is not served yet: the Service's address is given back when the Service goes"})
	}
	switch s.SessionAffinity {
	case "", "None":
	case "ClientIP":
		if o.AffinityTimeout = s.SessionAffinityTimeout; o.AffinityTimeout == 0 {
			o.AffinityTimeout = affinityDefault
		}
		if o.AffinityTimeout < 1 || o.AffinityTimeout > affinityMax {
			return Options{}, ReasonInvalid, fmt.Sprintf("sessionAffinityConfig.clientIP.timeoutSeconds must be 1 to %d", affinityMax)
		}
	default:
		return Options{}, ReasonInvalid, "sessionAffinity must be None or ClientIP"
	}

	// What is left: names of the two prefixes that are no knob here.
	known := map[string]bool{InternalAnnotation: true}
	for knob, alias := range aliases {
		known[OwnPrefix+knob], known[AliasPrefix+alias] = true, true
	}
	// OCCM's fourth timeout has no counterpart: HAProxy inspects nothing of a TCP stream here. Its default, 0,
	// asks for nothing.
	if strings.TrimSpace(ann[AliasPrefix+"timeout-tcp-inspect"]) == "0" {
		known[AliasPrefix+"timeout-tcp-inspect"] = true
	}
	var unknown []string
	for k := range ann {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(invalid) > 0 {
		o.Events = append(o.Events, Event{Type: "Warning", Reason: ReasonInvalidAnnotation,
			Message: "Not obeyed, the default holds: " + strings.Join(invalid, "; ")})
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		o.Events = append(o.Events, Event{Type: "Warning", Reason: ReasonAnnotationNotSupported,
			Message: "The load balancer does not obey these annotations: " + named(unknown) +
				". The Kubernetes guide lists the ones it obeys"})
	}
	return o, "", ""
}
