package lbgw

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const v2 = "PVEAPIToken=lb@proxcloud!c1=s1"

func annotated(kv ...string) *Service {
	s := web("u")
	s.Annotations = map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		s.Annotations[kv[i]] = kv[i+1]
	}
	return &s
}

// Every knob gives its option under its one name, proxcloud.io/lb-<knob>. "true" for the PROXY header is version 2,
// as v0.11.0 shipped it.
func TestEveryAnnotationByItsName(t *testing.T) {
	cases := []struct {
		knob, value string
		want        Options
	}{
		// The three of TLS on the balancer (tls_test.go): the Secrets are the controller's to read, and the other
		// two say nothing of a Service that came without certificates.
		{KnobTLSSecret, "web-tls", Options{}},
		{KnobTLSPorts, "", Options{}},
		{KnobBackendProtocol, "tcp", Options{TLSBackendTCP: true}},
		{KnobProxyProtocol, "v1", Options{ProxyVersion: 1}},
		{KnobProxyProtocol, "v2", Options{ProxyVersion: 2}},
		{KnobProxyProtocol, "true", Options{ProxyVersion: 2}},
		{KnobProxyProtocol, "false", Options{}},
		{KnobMethod, "leastconn", Options{Method: "leastconn"}},
		{KnobMethod, "LEAST_CONNECTIONS", Options{Method: "leastconn"}},
		{KnobMethod, "ROUND_ROBIN", Options{Method: "roundrobin"}},
		{KnobMethod, "SOURCE_IP", Options{Method: "source"}},
		{KnobMethod, "source", Options{Method: "source"}},
		{KnobConnectionLimit, "500", Options{ConnectionLimit: 500}},
		{KnobConnectionLimit, "-1", Options{}},
		{KnobTimeoutClientData, "50000", Options{TimeoutClientData: 50000}},
		{KnobTimeoutMemberConnect, "5000", Options{TimeoutMemberConnect: 5000}},
		{KnobTimeoutMemberData, "60000", Options{TimeoutMemberData: 60000}},
		{KnobXForwardedFor, "true", Options{XForwardedFor: true}},
		{KnobEnableHealthMonitor, "false", Options{NoMonitor: true}},
		{KnobEnableHealthMonitor, "true", Options{}},
		{KnobHealthDelay, "5", Options{HealthDelay: 5}},
		{KnobHealthTimeout, "3", Options{HealthTimeout: 3}},
		{KnobHealthMaxRetries, "1", Options{HealthRise: 1}},
		{KnobHealthMaxRetriesDown, "3", Options{HealthFall: 3}},
		{KnobNodeSelector, "pool=edge, public", Options{}},    // the controller's to read: nothing here, and no Event
		{KnobInternal, "false", Options{}},                    // "true" refuses: TestAnnotationsThatRefuseTheService
		{KnobKeepAddress, "true", Options{KeepAddress: true}}, // served by the portal, and no Event: TestKeepAddressAndLoadBalancerIP
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.knob] = true
		got, reason, msg := ServiceOptions(annotated(OwnPrefix+c.knob, c.value))
		if reason != "" || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q = %+v (%s %s), want %+v", c.knob, c.value, got, reason, msg, c.want)
		}
	}
	for _, knob := range knobs {
		if !seen[knob] {
			t.Errorf("knob %s has no case here", knob)
		}
	}
	// Protocol 1's flag is version 2, and an annotation the portal reads itself overrides it.
	old := web("u")
	old.ProxyProtocol = true
	if o, _, _ := ServiceOptions(&old); o.ProxyVersion != 2 || o.IPMode(&old) != "Proxy" {
		t.Errorf("protocol 1 flag: %+v", o)
	}
	old.Annotations = map[string]string{OwnPrefix + "proxy-protocol": "v1"}
	if o, _, _ := ServiceOptions(&old); o.ProxyVersion != 1 {
		t.Errorf("flag and annotation: %+v", o)
	}
	// A UDP Service has no PROXY header to announce.
	udp := web("u", Port{Protocol: "UDP", Port: 53, NodePort: 30053})
	if o := (Options{ProxyVersion: 2}); o.IPMode(&udp) != "VIP" {
		t.Error("ipMode Proxy on a Service without a TCP port")
	}
}

// What is not obeyed is said: every other annotation of the prefix in ONE Event, a value that is not understood in
// another, an unknown method under its own reason. The Service is served.
func TestAnnotationsThatAreNotObeyed(t *testing.T) {
	// Short keys the upstream controller reads and this balancer has no counterpart for, under the own prefix.
	other := []string{"availability-zone", "class", "flavor-id", "hostname", "keep-floatingip", "lb-method", "load-balancer-address",
		"load-balancer-id", "network-id", "port-id", "subnet-id", "timeout-tcp-inspect", "default-tls-container-ref"}
	var kv, names []string
	for _, n := range other {
		kv, names = append(kv, OwnPrefix+n, "x"), append(names, OwnPrefix+n)
	}
	s := annotated(kv...)
	o, reason, _ := ServiceOptions(s)
	if reason != "" || len(o.Events) != 1 || o.Events[0].Reason != ReasonAnnotationNotSupported || o.Events[0].Type != "Warning" {
		t.Fatalf("%s %+v", reason, o.Events)
	}
	slices.Sort(names)
	if !strings.Contains(o.Events[0].Message, strings.Join(names, ", ")) {
		t.Fatalf("the Event does not name them all, sorted: %s", o.Events[0].Message)
	}

	for name, value := range map[string]string{
		OwnPrefix + "connection-limit": "0", OwnPrefix + "timeout-client-data": "999", OwnPrefix + "timeout-member-data": "fast",
		OwnPrefix + "health-monitor-delay": "301", OwnPrefix + "health-monitor-max-retries": "11", OwnPrefix + "x-forwarded-for": "yes",
		OwnPrefix + "proxy-protocol": "v3", OwnPrefix + "enable-health-monitor": "off",
	} {
		o, reason, _ := ServiceOptions(annotated(name, value))
		if reason != "" || len(o.Events) != 1 || o.Events[0].Reason != ReasonInvalidAnnotation || !strings.Contains(o.Events[0].Message, name) ||
			!strings.Contains(o.Events[0].Message, strconv.Quote(value)) {
			t.Errorf("%s=%s: %s %+v", name, value, reason, o.Events)
		}
		o.Events = nil
		if !reflect.DeepEqual(o, Options{}) {
			t.Errorf("%s=%s set something: %+v", name, value, o)
		}
	}
	o, reason, _ = ServiceOptions(annotated(OwnPrefix+"method", "SOURCE_IP_PORT"))
	if reason != "" || o.Method != "" || len(o.Events) != 1 || o.Events[0].Reason != ReasonLbMethodUnknown {
		t.Errorf("method: %s %+v", reason, o)
	}
	// Policy Local keeps its check, and says so.
	local := annotated(OwnPrefix+"enable-health-monitor", "false")
	local.ExternalTrafficPolicy = "Local"
	if o, _, _ := ServiceOptions(local); o.NoMonitor || len(o.Events) != 1 || o.Events[0].Reason != ReasonInvalidAnnotation {
		t.Errorf("policy Local without a check: %+v", o)
	}
}

// No name but proxcloud.io/lb-<knob> is read. Annotations of any other prefix, the upstream controller's among them,
// are not the load balancer's: nothing is obeyed, nothing is named, no Event is raised, and they are not sent
// (SentAnnotation). That holds for the upstream request for an internal balancer too: such a Service is served with a
// public address until it is renamed, which the guide says in so many words.
func TestOnlyTheOwnPrefixIsRead(t *testing.T) {
	// The rule is by prefix, so any prefix stands for every one that is not the product's.
	const other, otherInternal = "loadbalancer.example.org/", "service.beta.kubernetes.io/example-internal-load-balancer"
	foreign := map[string]string{other + "proxy-protocol": "true", other + "lb-method": "SOURCE_IP", other + "x-forwarded-for": "true",
		other + "connection-limit": "5", other + "flavor-id": "f1", otherInternal: "true", "example.com/owner": "team",
		"proxcloud.io/lbx-method": "source", "proxcloud.io/internal": "true"}
	for k := range foreign {
		if SentAnnotation(k) {
			t.Errorf("%s would be sent", k)
		}
	}
	if !SentAnnotation(InternalAnnotation) || InternalAnnotation != "proxcloud.io/lb-internal" {
		t.Errorf("the internal annotation is %s", InternalAnnotation)
	}
	// A request that carries them all the same (a controller of another make, or by hand): as if they were not there.
	s := web("u")
	s.Annotations = foreign
	if o, reason, msg := ServiceOptions(&s); reason != "" || !reflect.DeepEqual(o, Options{}) {
		t.Errorf("annotations of other prefixes: %+v %s %s, want nothing obeyed, nothing said and no refusal", o, reason, msg)
	}
	if read, long := Read(foreign); len(read) != 0 || len(long) != 0 {
		t.Errorf("read %v, too long %v", read, long)
	}
	if Internal(foreign) {
		t.Error("an internal request under another name is taken for the own one")
	}
}

func TestAnnotationsThatRefuseTheService(t *testing.T) {
	affinity := web("u")
	affinity.SessionAffinity = "ClientIP"
	affinity.SessionAffinityTimeout = 86401
	odd := web("u")
	odd.SessionAffinity = "Sticky"
	for name, c := range map[string]struct {
		s    *Service
		want string
	}{
		"internal":            {annotated(InternalAnnotation, "true"), ReasonInternalNotSupported},
		"internal, as a bool": {annotated(InternalAnnotation, "True"), ReasonInternalNotSupported},
		"internal, spaces":    {annotated(InternalAnnotation, " 1\n"), ReasonInternalNotSupported},
		"affinity timeout":    {&affinity, ReasonInvalid},
		"affinity type":       {&odd, ReasonInvalid},
	} {
		if _, reason, msg := ServiceOptions(c.s); reason != c.want || msg == "" {
			t.Errorf("%s: %q %q, want %s", name, reason, msg, c.want)
		}
	}
	if _, _, msg := ServiceOptions(annotated(InternalAnnotation, "true")); !strings.Contains(msg, "remove proxcloud.io/lb-internal") {
		t.Errorf("the refusal does not name the annotation to remove: %s", msg)
	}
	// The annotation guards an address, so it fails closed: a value that is no boolean may have meant true, and the
	// Service is refused and told the value. It was served on a public address with a Warning.
	for _, value := range []string{"yes", "on", "", "  ", "tru", strings.Repeat("x", MaxAnnotationValue+1)} {
		s := annotated(InternalAnnotation, value)
		_, reason, msg := ServiceOptions(s)
		said := strings.TrimSpace(value)
		if len(said) > 64 {
			said = said[:64]
		}
		if !Internal(s.Annotations) || reason != ReasonInvalidAnnotation || !strings.Contains(msg, InternalAnnotation+`: "`+said) ||
			!strings.Contains(msg, "no public address") || len(msg) > 256 {
			t.Errorf("internal %q is served: %q %q", value, reason, msg)
		}
	}
	for _, value := range []string{"false", "False", "0", " f\n"} {
		s := annotated(InternalAnnotation, value)
		if o, reason, _ := ServiceOptions(s); Internal(s.Annotations) || reason != "" || len(o.Events) != 0 {
			t.Errorf("internal %q: %s %+v", value, reason, o.Events)
		}
	}
	if Internal(nil) || Internal(map[string]string{OwnPrefix + KnobMethod: "source"}) {
		t.Error("a Service without the annotation is taken for internal")
	}
	ok := web("u")
	ok.SessionAffinity = "ClientIP"
	if o, reason, _ := ServiceOptions(&ok); reason != "" || o.AffinityTimeout != 10800 {
		t.Errorf("ClientIP without a time: %+v %s, want Kubernetes' three hours", o, reason)
	}
	ok.SessionAffinityTimeout = 600
	if o, _, _ := ServiceOptions(&ok); o.AffinityTimeout != 600 {
		t.Errorf("ClientIP 600: %+v", o)
	}
}

// Protocol 2 at the gateway: UDP and a TCP and a UDP port of one number are served, the answer says the ipMode and
// the Events, and a Service that asks for an internal balancer is refused before the portal, so it never gets a
// public address. A protocol 1 controller's UDP Service stays refused, whatever the portal can do: the portal's
// upgrade alone must not hand out an address that is billed.
func TestProtocol2(t *testing.T) {
	udp := web("udp", Port{Protocol: "UDP", Port: 53, NodePort: 30053})
	mixed := web("mixed", Port{Protocol: "TCP", Port: 53, NodePort: 30053}, Port{Protocol: "UDP", Port: 53, NodePort: 30053})
	twice := web("twice", Port{Protocol: "UDP", Port: 53, NodePort: 30053}, Port{Protocol: "UDP", Port: 53, NodePort: 30054})
	sctp := web("sctp", Port{Protocol: "SCTP", Port: 9, NodePort: 30009})
	internal := web("internal")
	internal.Annotations = map[string]string{InternalAnnotation: "true"}
	proxy := web("proxy")
	proxy.Annotations = map[string]string{OwnPrefix + "proxy-protocol": "v1", OwnPrefix + "flavor-id": "f1", OwnPrefix + "method": "SOURCE_IP_PORT"}
	sel := web("selected")
	sel.Nodes = &[]string{"10.0.0.11"}
	badSel := web("badsel")
	badSel.Nodes = &[]string{"w1"}
	body := Request{Services: []Service{udp, mixed, twice, sctp, internal, proxy, sel, badSel, web("plain")}}

	r := newRig()
	r.protocol = "2"
	w, ans := r.put(t, v2, body)
	if w.Code != http.StatusOK || w.Header().Get(ProtocolHeader) != "2" {
		t.Fatalf("%d %s, protocol %q", w.Code, w.Body, w.Header().Get(ProtocolHeader))
	}
	want := map[string]string{"udp": "", "mixed": "", "twice": ReasonInvalid, "sctp": ReasonProtocolNotSupported, "internal": ReasonInternalNotSupported,
		"proxy": "", "selected": "", "badsel": ReasonInvalid, "plain": ""}
	for _, s := range ans.Services {
		if s.Reason != want[s.UID] || (s.Reason != "") != (s.State == StateRefused) {
			t.Errorf("%+v, want reason %q", s, want[s.UID])
		}
		if s.State == StateRefused && (s.IPMode != "" || s.Events != nil) {
			t.Errorf("a refused Service has an ipMode or Events: %+v", s)
		}
	}
	by := map[string]ServiceStatus{}
	for _, s := range ans.Services {
		by[s.UID] = s
	}
	if by["plain"].IPMode != "VIP" || by["udp"].IPMode != "VIP" || by["proxy"].IPMode != "Proxy" {
		t.Errorf("ipMode: %+v", by)
	}
	if ev := by["proxy"].Events; len(ev) != 2 || ev[0].Reason != ReasonLbMethodUnknown || ev[1].Reason != ReasonAnnotationNotSupported ||
		!strings.Contains(ev[1].Message, OwnPrefix+"flavor-id") {
		t.Errorf("events: %+v", ev)
	}
	for _, s := range r.portal.got {
		if s.UID == "internal" || s.UID == "sctp" {
			t.Fatalf("a refused Service reached the portal: %+v", s)
		}
	}
	if len(r.portal.got) != 5 {
		t.Fatalf("reached the portal: %+v", r.portal.got)
	}

	// The same body from a controller of v0.11.0.
	old := newRig()
	_, ans = old.put(t, v2, body)
	for _, s := range ans.Services {
		switch s.UID {
		case "udp", "mixed":
			if s.Reason != ReasonUDPNotSupported || !strings.HasSuffix(s.Message, "upgrade the cluster") {
				t.Errorf("protocol 1: %+v", s)
			}
		}
		if s.IPMode != "" || s.Events != nil {
			t.Errorf("protocol 1 was answered a protocol 2 field: %+v", s)
		}
	}
	for _, s := range old.portal.got {
		if s.UID == "udp" || s.UID == "mixed" {
			t.Fatalf("protocol 1: a UDP Service reached the portal, where it would get an address")
		}
	}
}

// A request as a controller of v0.11.0 wrote it, byte for byte, is taken and reaches the portal as it did.
func TestProtocol1RequestIsServedAsBefore(t *testing.T) {
	const body = `{"services":[{"uid":"u-1","namespace":"web","name":"shop","ports":[{"name":"http","protocol":"TCP","port":80,"nodePort":30080}],` +
		`"externalTrafficPolicy":"Local","healthCheckNodePort":31000,"sourceRanges":["198.51.100.0/24"],"proxyProtocol":true,"ipFamilies":["IPv4"]}],` +
		`"nodes":[{"name":"w1","address":"10.0.0.11"}]}`
	r := newRig()
	w, ans := r.put(t, v2, json.RawMessage(body))
	if w.Code != http.StatusOK || len(ans.Services) != 1 || ans.Services[0].State != StateBuilding {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := Service{UID: "u-1", Namespace: "web", Name: "shop", Ports: []Port{{Name: "http", Protocol: "TCP", Port: 80, NodePort: 30080}},
		ExternalTrafficPolicy: "Local", HealthCheckNodePort: 31000, SourceRanges: []string{"198.51.100.0/24"}, ProxyProtocol: true, IPFamilies: []string{"IPv4"}}
	if len(r.portal.got) != 1 || !reflect.DeepEqual(r.portal.got[0], want) {
		t.Fatalf("reached the portal: %+v\nwant %+v", r.portal.got, want)
	}
	if strings.Contains(w.Body.String(), "ipMode") || strings.Contains(w.Body.String(), "events") {
		t.Fatalf("the answer to protocol 1 holds a protocol 2 field: %s", w.Body)
	}
}

// An annotation that cannot be read never refuses a Service, so a Service that v0.11.0 served with such an annotation
// left on it keeps its address: a value that is too long is as if it were not set, any number of names the table
// does not know is named and counted, and a name that is not the load balancer's counts for nothing. Each kind is
// one Event with the same text every turn. The internal annotation still refuses.
func TestAnnotationsThatCannotBeReadAreNotObeyed(t *testing.T) {
	long := strings.Repeat("x", MaxAnnotationValue+1)
	leftover := func() *Service {
		s := annotated(OwnPrefix+KnobMethod, long, OwnPrefix+KnobConnectionLimit, "500",
			OwnPrefix+"zz", long, "example.com/owner", long)
		for i := range 40 {
			s.Annotations[OwnPrefix+"x"+strconv.Itoa(10+i)] = "1"
		}
		return s
	}
	o, reason, msg := ServiceOptions(leftover())
	if reason != "" {
		t.Fatalf("refused: %s %s", reason, msg)
	}
	// The method's value cannot be read, so the default holds; the limit is read.
	if o.Method != "" || o.ConnectionLimit != 500 {
		t.Errorf("the annotations that can be read: %+v", o)
	}
	if len(o.Events) != 2 || o.Events[0].Reason != ReasonInvalidAnnotation || o.Events[1].Reason != ReasonAnnotationNotSupported {
		t.Fatalf("events: %+v", o.Events)
	}
	if got, want := o.Events[0].Message, "Not obeyed, the default holds: "+OwnPrefix+"method, "+OwnPrefix+"zz: longer than 256 characters"; got != want {
		t.Errorf("too long:\n got %s\nwant %s", got, want)
	}
	unknown := o.Events[1].Message
	if !strings.Contains(unknown, OwnPrefix+"x10, ") || !strings.Contains(unknown, OwnPrefix+"x41 and 8 more.") ||
		strings.Contains(unknown, OwnPrefix+"x42") || strings.Contains(unknown, "example.com") || strings.Contains(unknown, OwnPrefix+"zz") {
		t.Errorf("not known: %s", unknown)
	}
	for range 20 { // a map's order must not reach the text: the controller says an Event again when it changes
		if again, _, _ := ServiceOptions(leftover()); !reflect.DeepEqual(again.Events, o.Events) {
			t.Fatalf("the Events differ between two readings:\n%+v\n%+v", again.Events, o.Events)
		}
	}
	internal := leftover()
	internal.Annotations[InternalAnnotation] = "true"
	if _, reason, _ := ServiceOptions(internal); reason != ReasonInternalNotSupported {
		t.Errorf("internal among annotations that cannot be read: %q", reason)
	}

	// Through the gateway: the Service reaches the portal, and its answer carries the two Events.
	r := newRig()
	r.protocol = "2"
	w, ans := r.put(t, v2, Request{Services: []Service{*leftover()}})
	if w.Code != http.StatusOK || len(r.portal.got) != 1 || ans.Services[0].State != StateBuilding || len(ans.Services[0].Events) != 2 {
		t.Fatalf("gateway: %d %+v, the portal got %d", w.Code, ans, len(r.portal.got))
	}
	// A state over the body's limit is not taken at all, and the answer says what stays and what to do.
	big := leftover()
	for i := range 300 {
		big.Annotations[OwnPrefix+"y"+strconv.Itoa(i)] = long[:MaxAnnotationValue]
	}
	w, _ = r.put(t, v2, Request{Services: []Service{*big}})
	if w.Code != http.StatusRequestEntityTooLarge || r.portal.calls != 1 || !strings.Contains(w.Body.String(), ReasonTooLarge) ||
		!strings.Contains(w.Body.String(), "the balancer stays as it is") {
		t.Fatalf("over the body limit: %d %s, portal calls %d", w.Code, w.Body, r.portal.calls)
	}
}

// The forwarded-for header asked for next to PROXY. OCCM fails such a Service's sync and leaves a balancer that exists
// as it was (loadbalancer.go:1627-1631). A refusal here would take the Service out of the state and give its address
// back, also of a Service that v0.11.0 served with its PROXY annotation while the other one lay on it unread. So the
// Service is served, PROXY holds, the header is not added, and the one Event names the annotation.
func TestForwardedForNextToProxyKeepsTheAddress(t *testing.T) {
	leftover := annotated(OwnPrefix+KnobProxyProtocol, "true", OwnPrefix+KnobXForwardedFor, "true")
	leftover.ProxyProtocol = true // as v0.11.0's controller read it, and this one still sends it
	for name, c := range map[string]struct {
		s       *Service
		version int
		named   string
	}{
		"own names":                          {annotated(OwnPrefix+KnobXForwardedFor, "true", OwnPrefix+KnobProxyProtocol, "true"), 2, OwnPrefix + KnobXForwardedFor},
		"version 1":                          {annotated(OwnPrefix+KnobXForwardedFor, "true", OwnPrefix+KnobProxyProtocol, "v1"), 1, OwnPrefix + KnobXForwardedFor},
		"v0.11.0's PROXY and a name left on": {leftover, 2, OwnPrefix + KnobXForwardedFor},
		"PROXY off: the header is added":     {annotated(OwnPrefix+KnobXForwardedFor, "true", OwnPrefix+KnobProxyProtocol, "false"), 0, ""},
		"the header off: nothing to be said": {annotated(OwnPrefix+KnobXForwardedFor, "false", OwnPrefix+KnobProxyProtocol, "v1"), 1, ""},
		"the header alone":                   {annotated(OwnPrefix+KnobXForwardedFor, "true"), 0, ""},
	} {
		o, reason, msg := ServiceOptions(c.s)
		if reason != "" {
			t.Errorf("%s: refused, so its address goes back: %s %s", name, reason, msg)
			continue
		}
		if o.ProxyVersion != c.version || o.XForwardedFor != (c.version == 0) {
			t.Errorf("%s: PROXY %d, forwarded-for %v", name, o.ProxyVersion, o.XForwardedFor)
		}
		if c.named == "" {
			if len(o.Events) != 0 {
				t.Errorf("%s: %+v", name, o.Events)
			}
			continue
		}
		if len(o.Events) != 1 || o.Events[0].Reason != ReasonInvalidAnnotation || o.Events[0].Type != "Warning" ||
			!strings.Contains(o.Events[0].Message, c.named+": the forwarded-for header is not added") {
			t.Errorf("%s: %+v", name, o.Events)
		}
	}

	// Through the gateway: the Service reaches the portal, so its address stays, and the answer says Proxy.
	r := newRig()
	r.protocol = "2"
	w, ans := r.put(t, v2, Request{Services: []Service{*leftover}})
	if w.Code != http.StatusOK || len(r.portal.got) != 1 || ans.Services[0].State == StateRefused || ans.Services[0].IPMode != "Proxy" ||
		len(ans.Services[0].Events) != 1 {
		t.Fatalf("gateway: %d %+v, the portal got %d", w.Code, ans, len(r.portal.got))
	}
}

// keep-address is a boolean like the other flags, read the same way by ServiceOptions and by KeepsAddress, which the
// portal asks when a Service leaves the state. spec.loadBalancerIP reaches the portal as an IPv4 address or not at
// all, and only from a controller of protocol 2: a text that is none is dropped with one Event that does not repeat
// it, and the Service is served.
func TestKeepAddressAndLoadBalancerIP(t *testing.T) {
	for value, want := range map[string]bool{"true": true, " True ": true, "1": true, "false": false, "": false, "yes": false,
		strings.Repeat("t", MaxAnnotationValue+1): false} {
		s := annotated(OwnPrefix+KnobKeepAddress, value)
		o, reason, _ := ServiceOptions(s)
		if reason != "" || o.KeepAddress != want || KeepsAddress(s) != want {
			t.Errorf("keep-address %q: options %v, KeepsAddress %v, want %v (%s)", value, o.KeepAddress, KeepsAddress(s), want, reason)
		}
		if slices.ContainsFunc(o.Events, func(ev Event) bool { return ev.Reason == ReasonKeepAddressLimit }) {
			t.Errorf("keep-address %q: the gateway knows no cluster's kept addresses: %+v", value, o.Events)
		}
	}
	if KeepsAddress(annotated("example.org/lb-keep-address", "true")) || KeepsAddress(&Service{}) {
		t.Error("keep-address is read under the load balancer's own name only")
	}

	r := newRig()
	r.protocol = "2"
	asked := func(ip string) Service {
		s := web("u-" + strconv.Itoa(len(ip)))
		s.LoadBalancerIP = ip
		return s
	}
	for ip, want := range map[string]string{"203.0.113.7": "203.0.113.7", "": "", "2001:db8::1": "", "203.0.113.007": "", "203.0.113.7/32": "",
		"kept-address <script>": "", "203.0.113.7 ": ""} {
		w, ans := r.put(t, v2, Request{Services: []Service{asked(ip)}})
		if w.Code != http.StatusOK || len(r.portal.got) != 1 || r.portal.got[0].LoadBalancerIP != want || ans.Services[0].State != StateBuilding {
			t.Fatalf("loadBalancerIP %q: %d %s, the portal got %+v, want %q", ip, w.Code, w.Body, r.portal.got, want)
		}
		said := slices.ContainsFunc(ans.Services[0].Events, func(ev Event) bool { return ev.Reason == ReasonLoadBalancerIPIgnored })
		if said != (ip != want) || (ip != "" && ip != want && strings.Contains(w.Body.String(), ip)) {
			t.Errorf("loadBalancerIP %q: answer %s", ip, w.Body)
		}
	}
	// A controller of protocol 1 never sent the field; one that did is not obeyed, and told nothing new.
	r.protocol = "1"
	w, _ := r.put(t, v2, Request{Services: []Service{asked("203.0.113.7")}})
	if w.Code != http.StatusOK || r.portal.got[0].LoadBalancerIP != "" || strings.Contains(w.Body.String(), "events") {
		t.Fatalf("protocol 1: %d %s, the portal got %+v", w.Code, w.Body, r.portal.got)
	}
}
