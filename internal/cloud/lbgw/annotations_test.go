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

// Every knob, by its own name and by OCCM's name, gives the same option; the one exception is the PROXY
// header's "true", version 2 under the own name (as v0.11.0 shipped it) and version 1 under OCCM's (as the
// OCCM reads it).
func TestEveryAnnotationByOwnNameAndByAlias(t *testing.T) {
	cases := []struct {
		knob, alias, value string
		want               Options
	}{
		{KnobProxyProtocol, "proxy-protocol", "v1", Options{ProxyVersion: 1}},
		{KnobProxyProtocol, "proxy-protocol", "v2", Options{ProxyVersion: 2}},
		{KnobProxyProtocol, "proxy-protocol", "false", Options{}},
		{KnobMethod, "lb-method", "leastconn", Options{Method: "leastconn"}},
		{KnobMethod, "lb-method", "LEAST_CONNECTIONS", Options{Method: "leastconn"}},
		{KnobMethod, "lb-method", "ROUND_ROBIN", Options{Method: "roundrobin"}},
		{KnobMethod, "lb-method", "SOURCE_IP", Options{Method: "source"}},
		{KnobMethod, "lb-method", "source", Options{Method: "source"}},
		{KnobConnectionLimit, "connection-limit", "500", Options{ConnectionLimit: 500}},
		{KnobConnectionLimit, "connection-limit", "-1", Options{}},
		{KnobTimeoutClientData, "timeout-client-data", "50000", Options{TimeoutClientData: 50000}},
		{KnobTimeoutMemberConnect, "timeout-member-connect", "5000", Options{TimeoutMemberConnect: 5000}},
		{KnobTimeoutMemberData, "timeout-member-data", "60000", Options{TimeoutMemberData: 60000}},
		{KnobXForwardedFor, "x-forwarded-for", "true", Options{XForwardedFor: true}},
		{KnobEnableHealthMonitor, "enable-health-monitor", "false", Options{NoMonitor: true}},
		{KnobEnableHealthMonitor, "enable-health-monitor", "true", Options{}},
		{KnobHealthDelay, "health-monitor-delay", "5", Options{HealthDelay: 5}},
		{KnobHealthTimeout, "health-monitor-timeout", "3", Options{HealthTimeout: 3}},
		{KnobHealthMaxRetries, "health-monitor-max-retries", "1", Options{HealthRise: 1}},
		{KnobHealthMaxRetriesDown, "health-monitor-max-retries-down", "3", Options{HealthFall: 3}},
		{KnobNodeSelector, "node-selector", "pool=edge, public", Options{}}, // the controller's to read: nothing here, and no Event
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.knob] = true
		if aliases[c.knob] != c.alias {
			t.Errorf("%s: alias %q, want %q", c.knob, aliases[c.knob], c.alias)
		}
		for _, name := range []string{OwnPrefix + c.knob, AliasPrefix + c.alias} {
			got, reason, msg := ServiceOptions(annotated(name, c.value))
			if reason != "" || !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s: %q = %+v (%s %s), want %+v", name, c.value, got, reason, msg, c.want)
			}
		}
	}
	// keep-address is read under both names and says that it is not served yet.
	seen[KnobKeepAddress] = true
	for _, name := range []string{OwnPrefix + "keep-address", AliasPrefix + "keep-floatingip"} {
		got, reason, _ := ServiceOptions(annotated(name, "true"))
		if reason != "" || !got.KeepAddress || len(got.Events) != 1 || got.Events[0].Reason != ReasonKeepAddressNotServed || !strings.Contains(got.Events[0].Message, name) {
			t.Errorf("%s: %+v %s", name, got, reason)
		}
	}
	for knob := range aliases {
		if !seen[knob] {
			t.Errorf("knob %s has no case here", knob)
		}
	}

	// "true": version 2 by the own name, version 1 by OCCM's. With both set the own name wins.
	if o, _, _ := ServiceOptions(annotated(OwnPrefix+"proxy-protocol", "true")); o.ProxyVersion != 2 {
		t.Errorf("own true = %+v", o)
	}
	if o, _, _ := ServiceOptions(annotated(AliasPrefix+"proxy-protocol", "true")); o.ProxyVersion != 1 {
		t.Errorf("alias true = %+v", o)
	}
	if o, _, _ := ServiceOptions(annotated(OwnPrefix+"method", "source", AliasPrefix+"lb-method", "LEAST_CONNECTIONS")); o.Method != "source" {
		t.Errorf("both names: %+v, want the own one", o)
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

// What is not obeyed is said: every other annotation of the two prefixes in ONE Event, a value that is not understood
// in another, an unknown method under OCCM's own reason. The Service is served.
func TestAnnotationsThatAreNotObeyed(t *testing.T) {
	other := []string{"availability-zone", "class", "default-tls-container-ref", "enable-ingress-hostname", "flavor-id", "floating-network-id",
		"floating-subnet", "floating-subnet-id", "floating-subnet-tags", "hostname", "listener-tags", "load-balancer-address", "load-balancer-id",
		"load-balancer-tags", "member-subnet-id", "network-id", "pool-tags", "port-id", "subnet-id", "timeout-tcp-inspect"}
	var kv, names []string
	for _, n := range other {
		kv, names = append(kv, AliasPrefix+n, "x"), append(names, AliasPrefix+n)
	}
	kv, names = append(kv, OwnPrefix+"tls-secret", "web-tls"), append(names, OwnPrefix+"tls-secret")
	s := annotated(kv...)
	o, reason, _ := ServiceOptions(s)
	if reason != "" || len(o.Events) != 1 || o.Events[0].Reason != ReasonAnnotationNotSupported || o.Events[0].Type != "Warning" {
		t.Fatalf("%s %+v", reason, o.Events)
	}
	slices.Sort(names)
	if !strings.Contains(o.Events[0].Message, strings.Join(names, ", ")) {
		t.Fatalf("the Event does not name them all, sorted: %s", o.Events[0].Message)
	}
	// OCCM's inspect timeout at its default asks for nothing.
	if o, _, _ := ServiceOptions(annotated(AliasPrefix+"timeout-tcp-inspect", "0")); len(o.Events) != 0 {
		t.Errorf("timeout-tcp-inspect 0: %+v", o.Events)
	}

	for name, value := range map[string]string{
		OwnPrefix + "connection-limit": "0", AliasPrefix + "connection-limit": "20001", OwnPrefix + "timeout-client-data": "999",
		AliasPrefix + "timeout-member-data": "fast", OwnPrefix + "health-monitor-delay": "301", AliasPrefix + "health-monitor-max-retries": "11",
		OwnPrefix + "x-forwarded-for": "yes", AliasPrefix + "proxy-protocol": "v3", OwnPrefix + "enable-health-monitor": "off",
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
	for _, name := range []string{OwnPrefix + "method", AliasPrefix + "lb-method"} {
		o, reason, _ := ServiceOptions(annotated(name, "SOURCE_IP_PORT"))
		if reason != "" || o.Method != "" || len(o.Events) != 1 || o.Events[0].Reason != ReasonLbMethodUnknown {
			t.Errorf("%s: %s %+v", name, reason, o)
		}
	}
	// Policy Local keeps its check, and says so.
	local := annotated(AliasPrefix+"enable-health-monitor", "false")
	local.ExternalTrafficPolicy = "Local"
	if o, _, _ := ServiceOptions(local); o.NoMonitor || len(o.Events) != 1 || o.Events[0].Reason != ReasonInvalidAnnotation {
		t.Errorf("policy Local without a check: %+v", o)
	}
}

func TestAnnotationsThatRefuseTheService(t *testing.T) {
	many := annotated()
	for i := range MaxAnnotations + 1 {
		many.Annotations[OwnPrefix+"x"+strconv.Itoa(i)] = "1"
	}
	affinity := web("u")
	affinity.SessionAffinity = "ClientIP"
	affinity.SessionAffinityTimeout = 86401
	odd := web("u")
	odd.SessionAffinity = "Sticky"
	for name, c := range map[string]struct {
		s    *Service
		want string
	}{
		"internal":                                      {annotated(InternalAnnotation, "true"), ReasonInternalNotSupported},
		"internal, as a bool":                           {annotated(InternalAnnotation, "True"), ReasonInternalNotSupported},
		"forwarded-for and PROXY, own names":            {annotated(OwnPrefix+"x-forwarded-for", "true", OwnPrefix+"proxy-protocol", "true"), ReasonInvalidAnnotation},
		"forwarded-for and PROXY, OCCM's names":         {annotated(AliasPrefix+"x-forwarded-for", "true", AliasPrefix+"proxy-protocol", "v2"), ReasonInvalidAnnotation},
		"too many":                                      {many, ReasonInvalidAnnotation},
		"a value too long":                              {annotated(OwnPrefix+"method", strings.Repeat("x", MaxAnnotationValue+1)), ReasonInvalidAnnotation},
		"a name that is not a load balancer annotation": {annotated("example.com/owner", "me"), ReasonInvalidAnnotation},
		"affinity timeout":                              {&affinity, ReasonInvalid},
		"affinity type":                                 {&odd, ReasonInvalid},
	} {
		if _, reason, msg := ServiceOptions(c.s); reason != c.want || msg == "" {
			t.Errorf("%s: %q %q, want %s", name, reason, msg, c.want)
		}
	}
	if _, reason, _ := ServiceOptions(annotated(InternalAnnotation, "false")); reason != "" {
		t.Errorf("internal false: %s", reason)
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
	proxy.Annotations = map[string]string{AliasPrefix + "proxy-protocol": "true", AliasPrefix + "flavor-id": "f1", AliasPrefix + "lb-method": "SOURCE_IP_PORT"}
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
		!strings.Contains(ev[1].Message, AliasPrefix+"flavor-id") {
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
