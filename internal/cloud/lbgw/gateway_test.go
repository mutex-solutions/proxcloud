package lbgw

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

type fakeReg struct {
	mu    sync.Mutex
	creds map[string]*Credential
	seen  int
}

func (f *fakeReg) Credential(_ context.Context, id string) (*Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.creds[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeReg) Seen(_ context.Context, id string, at time.Time, protocol int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen++
	f.creds[id].LastSeenAt, f.creds[id].Protocol = &at, protocol
	return nil
}

// fakePortal accepts every Service as building and records what reached it.
type fakePortal struct {
	got     []Service
	nodes   []Node
	certs   []CertificateData
	need    []string // what it asks for
	gone    []Gone
	refusal *Refusal
	calls   int
}

func (p *fakePortal) LBClusterState(_ context.Context, cred *Credential, services []Service, nodes []Node, certs []CertificateData, gone []Gone) (*Answer, error) {
	p.calls++
	if p.refusal != nil {
		return nil, p.refusal
	}
	p.got, p.nodes, p.certs, p.gone = services, nodes, certs, gone
	ans := &Answer{NeedCertificates: p.need}
	for _, s := range services {
		ans.Services = append(ans.Services, ServiceStatus{UID: s.UID, State: StateBuilding})
	}
	return ans, nil
}

type rig struct {
	h      *Handler
	reg    *fakeReg
	portal *fakePortal
	events []audit.AuditEvent
	// protocol is what the controller says it speaks; "" is "1", a controller of v0.11.0.
	protocol string
}

func newRig() *rig {
	now := time.Now().UTC()
	r := &rig{reg: &fakeReg{creds: map[string]*Credential{
		"c1":   {ID: "c1", ProjectID: "p1", SecretSHA256: hashSecret("s1")},
		"c2":   {ID: "c2", ProjectID: "p2", SecretSHA256: hashSecret("s2")},
		"gone": {ID: "gone", ProjectID: "p1", SecretSHA256: hashSecret("s3"), RevokedAt: &now},
	}}, portal: &fakePortal{}}
	r.h = &Handler{Reg: r.reg, Portal: r.portal, Audit: func(ev audit.AuditEvent) { r.events = append(r.events, ev) },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return r
}

func (r *rig) put(t *testing.T, auth string, body any) (*httptest.ResponseRecorder, Answer) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, StatePath, bytes.NewReader(raw))
	req.Header.Set("Authorization", auth)
	req.Header.Set(ProtocolHeader, cmp.Or(r.protocol, "1"))
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	var ans Answer
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	return w, ans
}

func web(uid string, ports ...Port) Service {
	if len(ports) == 0 {
		ports = []Port{{Protocol: "TCP", Port: 80, NodePort: 30080}}
	}
	return Service{UID: uid, Namespace: "default", Name: "web", Ports: ports}
}

// Only a cluster's own live credential passes: another realm's token, a revoked one, a wrong secret, another cluster's
// secret. Nothing reaches the portal for any of them.
func TestAuthentication(t *testing.T) {
	r := newRig()
	body := Request{Services: []Service{web("u1")}}
	for name, auth := range map[string]string{
		"none":            "",
		"csi realm":       "PVEAPIToken=csi@proxcloud!c1=s1",
		"revoked":         "PVEAPIToken=lb@proxcloud!gone=s3",
		"wrong secret":    "PVEAPIToken=lb@proxcloud!c1=nope",
		"another cluster": "PVEAPIToken=lb@proxcloud!c1=s2",
		"unknown cluster": "PVEAPIToken=lb@proxcloud!c9=s1",
		"bad id":          "PVEAPIToken=lb@proxcloud!C1/../c2=s1",
	} {
		if w, _ := r.put(t, auth, body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if r.portal.calls != 0 {
		t.Fatalf("a refused credential reached the portal %d times", r.portal.calls)
	}
	w, ans := r.put(t, "PVEAPIToken=lb@proxcloud!c1=s1", body)
	if w.Code != http.StatusOK || len(ans.Services) != 1 || ans.Services[0].State != StateBuilding || w.Header().Get(ProtocolHeader) != strconv.Itoa(Protocol) {
		t.Fatalf("own credential: %d %s", w.Code, w.Body)
	}
	// The last call is written at most once a minute.
	r.put(t, "PVEAPIToken lb@proxcloud!c1=s1", body)
	if r.reg.seen != 1 {
		t.Fatalf("last seen written %d times", r.reg.seen)
	}
}

// The protocol a controller names is kept with its last call, which is how the consoles tell an old controller: it is
// written with the first call, at once when it changes (a cluster bumped to the portal's chart), and not again within
// the minute while it stays. A credential that is refused leaves it alone.
func TestControllerProtocolIsRecorded(t *testing.T) {
	r := newRig()
	const own = "PVEAPIToken=lb@proxcloud!c1=s1"
	body := Request{Services: []Service{web("u1")}}
	for i, c := range []struct {
		protocol, auth string
		want, writes   int
	}{
		{"1", own, 1, 1},
		{"1", own, 1, 1},
		{"2", "PVEAPIToken=lb@proxcloud!c1=nope", 1, 1},
		{"2", own, 2, 2},
		{"2", own, 2, 2},
		{"0", own, 0, 3}, // refused as too old, and shown as it named itself
		// A header that is no protocol is 0 too, so nothing is written for it: out of range, negative, not a number.
		{"99999999999999999999", own, 0, 3},
		{"-1", own, 0, 3},
		{"two", own, 0, 3},
	} {
		r.protocol = c.protocol
		r.put(t, c.auth, body)
		if got := r.reg.creds["c1"].Protocol; got != c.want || r.reg.seen != c.writes {
			t.Fatalf("call %d (protocol %s): the credential says protocol %d after %d writes, want %d after %d", i, c.protocol, got, r.reg.seen, c.want, c.writes)
		}
	}
}

// N1-3 (v0.11.0-rc.1 drill): a controller that kept its old Secret after a rotation was refused three times, and the
// log line named neither its cluster nor the reason, and no audit event was written. Every refusal is logged with the
// cluster it claimed and why. A known cluster's revoked credential and a wrong secret are audit events, one per
// cluster and reason a minute however often the controller comes back; what names no known cluster is not, and
// neither the log nor the event ever holds the secret.
func TestRefusedCredentialIsLoggedAndAudited(t *testing.T) {
	r := newRig()
	var logged bytes.Buffer
	r.h.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	at := time.Now()
	r.h.now = func() time.Time { return at }
	body := Request{}
	for _, tc := range []struct{ auth, cluster, why string }{
		{"", "", denyMalformed},
		{"Bearer x", "", denyMalformed},
		{"PVEAPIToken=csi@proxcloud!c1=TOPSECRET", "", denyMalformed},
		{"PVEAPIToken=lb@proxcloud!C1/../c2=TOPSECRET", "", denyMalformed},
		{"PVEAPIToken=lb@proxcloud!c1=", "c1", denyMalformed},
		{"PVEAPIToken=lb@proxcloud!c9=TOPSECRET", "c9", denyUnknown},
		{"PVEAPIToken=lb@proxcloud!gone=s3", "gone", denyRevoked},
		{"PVEAPIToken=lb@proxcloud!c1=TOPSECRET", "c1", denyWrongSecret},
		{"PVEAPIToken=lb@proxcloud!c1=s2", "c1", denyWrongSecret}, // another cluster's secret
	} {
		logged.Reset()
		w, _ := r.put(t, tc.auth, body)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"message":"authentication failure"`) || strings.Contains(w.Body.String(), tc.why) {
			t.Fatalf("%q: %d %s: every refusal is the same 401", tc.auth, w.Code, w.Body)
		}
		line := logged.String()
		var rec map[string]any
		if err := json.Unmarshal(logged.Bytes(), &rec); err != nil || rec["cluster"] != tc.cluster || rec["reason"] != tc.why || rec["remote"] == "" {
			t.Errorf("%q: log line %q, want cluster %q and reason %q", tc.auth, line, tc.cluster, tc.why)
		}
		if strings.Contains(line, "TOPSECRET") || strings.Contains(line, "s3") || strings.Contains(line, "../") {
			t.Errorf("%q: the log line holds the secret or the caller's own text: %q", tc.auth, line)
		}
	}
	// The controller of the released cluster and the guesser come back every ten seconds for a minute.
	for i := 0; i < 5; i++ {
		at = at.Add(10 * time.Second)
		r.put(t, "PVEAPIToken=lb@proxcloud!gone=s3", body)
		r.put(t, "PVEAPIToken=lb@proxcloud!c1=TOPSECRET", body)
	}
	want := map[string]string{"gone": denyRevoked, "c1": denyWrongSecret}
	if len(r.events) != 2 {
		t.Fatalf("audit events %d, want one per known cluster and reason: %+v", len(r.events), r.events)
	}
	for _, ev := range r.events {
		raw, _ := json.Marshal(ev)
		if ev.RequestInterface != audit.InterfaceLBGateway || ev.Action != audit.ActionDeny || ev.Outcome != audit.OutcomeFailure ||
			ev.ErrorMessage != "credential refused: "+want[ev.ResourceID] || ev.ResourceMetadata["reason"] != want[ev.ResourceID] ||
			ev.ProjectID == "" || strings.Contains(string(raw), "TOPSECRET") || strings.Contains(string(raw), `"s3"`) {
			t.Errorf("audit event: %s", raw)
		}
	}
	// A minute after the first: one more each. Another reason for the same cluster is its own event.
	at = at.Add(15 * time.Second)
	r.put(t, "PVEAPIToken=lb@proxcloud!gone=s3", body)
	r.put(t, "PVEAPIToken=lb@proxcloud!c1=TOPSECRET", body)
	if len(r.events) != 4 {
		t.Fatalf("audit events after a minute: %d, want 4", len(r.events))
	}
	if r.portal.calls != 0 || r.reg.seen != 0 {
		t.Fatalf("a refused credential reached the portal (%d) or counted as seen (%d)", r.portal.calls, r.reg.seen)
	}
}

// Each Service is checked on its own; a refused one never reaches the portal, the others do, and the answer keeps the
// request's order.
func TestServiceChecks(t *testing.T) {
	r := newRig()
	local := web("local")
	local.ExternalTrafficPolicy, local.HealthCheckNodePort = "Local", 31000
	anyRange := web("any")
	anyRange.SourceRanges = []string{"10.0.0.0/8", "0.0.0.0/0"}
	ranged := web("ranged")
	ranged.SourceRanges = []string{"203.0.113.7", "198.51.100.0/24"}
	v6 := web("v6")
	v6.IPFamilies = []string{"IPv4", "IPv6"}
	v6Range := web("v6range")
	v6Range.SourceRanges = []string{"2001:db8::/32"}
	long := web("long")
	for i := range 50 {
		long.SourceRanges = append(long.SourceRanges, "10.0."+strconv.Itoa(i)+".0/24")
	}
	noHC := web("nohc")
	noHC.ExternalTrafficPolicy = "Local"
	body := Request{Services: []Service{
		web("ok"),
		web("udp", Port{Protocol: "UDP", Port: 53, NodePort: 30053}),
		web("mixed", Port{Protocol: "TCP", Port: 53, NodePort: 30053}, Port{Protocol: "UDP", Port: 53, NodePort: 30054}),
		web("sctp", Port{Protocol: "SCTP", Port: 9, NodePort: 30009}),
		web("nonodeport", Port{Port: 80}),
		web("outofrange", Port{Port: 80, NodePort: 8080}),
		local, noHC, anyRange, ranged, v6, v6Range, long,
	}}
	w, ans := r.put(t, "PVEAPIToken=lb@proxcloud!c1=s1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := map[string]string{"ok": "", "udp": ReasonUDPNotSupported, "mixed": ReasonUDPNotSupported, "sctp": ReasonProtocolNotSupported,
		"nonodeport": ReasonNoNodePort, "outofrange": ReasonNoNodePort, "local": "", "nohc": ReasonNoNodePort, "any": "", "ranged": "",
		"v6": ReasonIPFamilyNotSupported, "v6range": ReasonIPFamilyNotSupported, "long": ReasonTooManySourceRanges}
	if len(ans.Services) != len(body.Services) {
		t.Fatalf("answer: %+v", ans.Services)
	}
	for i, s := range ans.Services {
		if s.UID != body.Services[i].UID || s.Reason != want[s.UID] || (s.Reason != "") != (s.State == StateRefused) {
			t.Errorf("%d: %+v, want reason %q", i, s, want[s.UID])
		}
	}
	got := map[string]Service{}
	for _, s := range r.portal.got {
		got[s.UID] = s
	}
	if len(got) != 4 || got["any"].SourceRanges != nil || strings.Join(got["ranged"].SourceRanges, ",") != "198.51.100.0/24,203.0.113.7/32" ||
		got["ok"].ExternalTrafficPolicy != "Cluster" || got["local"].HealthCheckNodePort != 31000 {
		t.Fatalf("reached the portal: %+v", r.portal.got)
	}
	// A refused Service is audited once, not once a poll; a new refusal is audited again.
	n := len(r.events)
	r.put(t, "PVEAPIToken=lb@proxcloud!c1=s1", body)
	if n != 1 || len(r.events) != 1 || r.events[0].RequestInterface != audit.InterfaceLBGateway || r.events[0].ProjectID != "p1" {
		t.Fatalf("audit: %d then %d events: %+v", n, len(r.events), r.events)
	}
	body.Services = body.Services[:1]
	body.Services = append(body.Services, web("udp2", Port{Protocol: "UDP", Port: 53, NodePort: 30053}))
	r.put(t, "PVEAPIToken=lb@proxcloud!c1=s1", body)
	if len(r.events) != 2 {
		t.Fatalf("a new refusal was not audited: %d events", len(r.events))
	}
}

// The request as a whole: the path, the method, an old controller, a body over 64 KiB or of another shape, more than
// fifty Services, a project the portal refuses. Each changes nothing; each one after authentication is audited.
func TestRequestRefusals(t *testing.T) {
	r := newRig()
	auth := "PVEAPIToken=lb@proxcloud!c1=s1"
	do := func(method, path, proto string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Authorization", auth)
		if proto != "" {
			req.Header.Set(ProtocolHeader, proto)
		}
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		return w
	}
	ok, _ := json.Marshal(Request{Services: []Service{web("u1")}})
	big := append([]byte(`{"services":[],"nodes":[],"pad":"`), bytes.Repeat([]byte("x"), 70<<10)...)
	for _, tc := range []struct {
		name, method, path, proto string
		body                      []byte
		status                    int
		reason                    string
	}{
		{"other path", http.MethodPut, "/lb/v1/other", "1", ok, http.StatusNotFound, ""},
		{"query", http.MethodPut, StatePath + "?project=p2", "1", ok, http.StatusNotFound, ""},
		{"get", http.MethodGet, StatePath, "1", nil, http.StatusMethodNotAllowed, ""},
		{"no protocol", http.MethodPut, StatePath, "", ok, http.StatusConflict, ReasonControllerTooOld},
		{"too large", http.MethodPut, StatePath, "1", big, http.StatusRequestEntityTooLarge, ReasonTooLarge},
		{"unknown field", http.MethodPut, StatePath, "1", []byte(`{"services":[],"projectId":"p2"}`), http.StatusBadRequest, ReasonInvalid},
		{"bad node", http.MethodPut, StatePath, "1", []byte(`{"nodes":[{"address":"fd00::1"}]}`), http.StatusBadRequest, ReasonInvalid},
		{"duplicate uid", http.MethodPut, StatePath, "1", []byte(`{"services":[{"uid":"a","ports":[{"port":80,"nodePort":30080}]},{"uid":"a","ports":[{"port":81,"nodePort":30081}]}]}`), http.StatusBadRequest, ReasonInvalid},
		{"name not a name", http.MethodPut, StatePath, "1", []byte(`{"services":[{"uid":"a","name":"x\ny","ports":[{"port":80,"nodePort":30080}]}]}`), http.StatusBadRequest, ReasonInvalid},
		{"old protocol", http.MethodPut, StatePath, "0", ok, http.StatusConflict, ReasonControllerTooOld},
	} {
		w := do(tc.method, tc.path, tc.proto, tc.body)
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != tc.status || got["reason"] != tc.reason {
			t.Errorf("%s: %d %s", tc.name, w.Code, w.Body)
		}
	}
	if r.portal.calls != 0 || len(r.events) != 10 {
		t.Fatalf("portal reached %d times, %d audit events", r.portal.calls, len(r.events))
	}
	// An old controller polling: one event, not one a poll.
	do(http.MethodPut, StatePath, "0", ok)
	do(http.MethodPut, StatePath, "", ok)
	if len(r.events) != 10 {
		t.Fatalf("a repeated refusal: %d audit events", len(r.events))
	}

	var many Request
	for i := range MaxServices + 1 {
		many.Services = append(many.Services, web("u"+strconv.Itoa(i)))
	}
	w, ans := r.put(t, auth, many)
	if w.Code != http.StatusOK || len(r.portal.got) != MaxServices || ans.Services[MaxServices].Reason != ReasonTooLarge {
		t.Fatalf("51 Services: %d, %d reached the portal", w.Code, len(r.portal.got))
	}

	r.portal.refusal = &Refusal{Status: http.StatusConflict, Reason: ReasonProjectNotEnabled, Message: "suspended"}
	if w, _ := r.put(t, auth, many); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), ReasonProjectNotEnabled) {
		t.Fatalf("suspended project: %d %s", w.Code, w.Body)
	}
}

// Known issue of v0.11.1 ("The gateway has no rate limit per cluster"; open finding 2 of the watch review): since the
// controller watches, a Service that flaps makes about one PUT a second, each one database work. A cluster has a
// bucket of rateBurst calls that gets one back every rateEvery. A call over it is answered 429 TooManyRequests with
// Retry-After before the body is read: nothing reaches the portal and nothing is written. The limit is the
// cluster's own: another cluster's call in the same instant passes (the control), and a refused credential that
// names the cluster takes none of its calls. A cluster that stays over it is logged and audited once a minute.
func TestClusterRateLimit(t *testing.T) {
	r := newRig()
	var logged bytes.Buffer
	r.h.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	at := time.Now()
	r.h.now = func() time.Time { return at }
	const c1, c2 = "PVEAPIToken=lb@proxcloud!c1=s1", "PVEAPIToken=lb@proxcloud!c2=s2"
	body := Request{Services: []Service{web("u1")}}
	said := func() (logs, events int) {
		for _, ev := range r.events {
			if ev.ResourceMetadata["reason"] == ReasonTooManyRequests {
				if ev.RequestInterface != audit.InterfaceLBGateway || ev.Action != audit.ActionDeny || ev.ProjectID != "p1" ||
					ev.ResourceID != "c1" || ev.ResourceMetadata["status"] != http.StatusTooManyRequests {
					t.Errorf("audit event: %+v", ev)
				}
				events++
			}
		}
		return strings.Count(logged.String(), `"msg":"lb gateway: rate limit"`), events
	}
	limited := func(w *httptest.ResponseRecorder, retryAfter string) {
		t.Helper()
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != retryAfter || got["reason"] != ReasonTooManyRequests ||
			!strings.Contains(got["message"], "Nothing changed; the next ones are taken in "+retryAfter+" s") {
			t.Fatalf("over the limit: %d, Retry-After %q, %s; want 429 and Retry-After %s", w.Code, w.Header().Get("Retry-After"), w.Body, retryAfter)
		}
	}

	// The whole bucket in one instant, and before each call one with a wrong secret for the same cluster.
	for i := range rateBurst {
		if w, _ := r.put(t, "PVEAPIToken=lb@proxcloud!c1=nope", body); w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong secret %d: %d %s", i, w.Code, w.Body)
		}
		if w, _ := r.put(t, c1, body); w.Code != http.StatusOK {
			t.Fatalf("call %d of %d: %d %s", i+1, rateBurst, w.Code, w.Body)
		}
	}
	calls, seen := r.portal.calls, r.reg.seen
	w, _ := r.put(t, c1, body)
	limited(w, "2")
	if w.Header().Get(ProtocolHeader) != strconv.Itoa(Protocol) {
		t.Errorf("the refusal names no protocol: %v", w.Header())
	}
	r.protocol = "2" // a protocol that changed is written at once, but not by a call over the limit
	w, _ = r.put(t, c1, body)
	limited(w, "2")
	r.protocol = ""
	if calls != rateBurst || r.portal.calls != calls || r.reg.seen != seen {
		t.Fatalf("the portal was called %d times for %d calls taken, and %d more for the ones over the limit; last seen written %d times more",
			calls, rateBurst, r.portal.calls-calls, r.reg.seen-seen)
	}
	if logs, events := said(); logs != 1 || events != 1 {
		t.Fatalf("two calls over the limit: %d log lines and %d audit events, want one of each", logs, events)
	}
	// The control: the other cluster, in the same instant.
	if w, _ := r.put(t, c2, body); w.Code != http.StatusOK || r.portal.calls != calls+1 {
		t.Fatalf("another cluster in the same instant: %d %s", w.Code, w.Body)
	}

	// One call comes back every rateEvery, not sooner.
	at = at.Add(rateEvery / 2)
	w, _ = r.put(t, c1, body)
	limited(w, "1")
	at = at.Add(rateEvery / 2)
	if w, _ := r.put(t, c1, body); w.Code != http.StatusOK || r.portal.calls != calls+2 {
		t.Fatalf("%s later: %d %s", rateEvery, w.Code, w.Body)
	}
	w, _ = r.put(t, c1, body)
	limited(w, "2")

	// The cluster keeps sending two calls a second: one in four is taken, the others are said once a minute.
	for range 57 {
		at = at.Add(time.Second)
		r.put(t, c1, body)
		r.put(t, c1, body)
	}
	if logs, events := said(); logs != 1 || events != 1 {
		t.Fatalf("59 s over the limit: %d log lines and %d audit events, want one of each", logs, events)
	}
	at = at.Add(time.Second)
	r.put(t, c1, body)
	w, _ = r.put(t, c1, body)
	limited(w, "2")
	if logs, events := said(); logs != 2 || events != 2 {
		t.Fatalf("a minute after the first: %d log lines and %d audit events, want two of each", logs, events)
	}

	// A cluster that was quiet for a minute has its whole bucket again, and no more than that.
	at = at.Add(rateBurst * rateEvery)
	for i := range rateBurst {
		if w, _ := r.put(t, c1, body); w.Code != http.StatusOK {
			t.Fatalf("after a quiet minute, call %d of %d: %d %s", i+1, rateBurst, w.Code, w.Body)
		}
	}
	w, _ = r.put(t, c1, body)
	limited(w, "2")
}

// The listener sends /lb/v1/ to the load balancer gateway and everything else, untouched, to the CSI gateway.
func TestSplit(t *testing.T) {
	var lb, csi []string
	h := Split(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { lb = append(lb, r.URL.Path) }),
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { csi = append(csi, r.URL.RawPath+r.URL.Path) }))
	for _, p := range []string{"/lb/v1/state", "/api2/json/nodes", "/lb/v2/state", "/lb/v1", "/api2/json//x"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, p, nil))
	}
	if strings.Join(lb, ",") != "/lb/v1/state" || strings.Join(csi, ",") != "/api2/json/nodes,/lb/v2/state,/lb/v1,/api2/json//x" {
		t.Fatalf("lb %v, csi %v", lb, csi)
	}
}

// The Services that are being deleted (Request.Gone, from v0.11.1-rc.3) reach the portal as the controller named
// them, beside the state and outside its size limit: a state that just fits must not keep a Service from being let
// go. A controller of protocol 1 has none, and what is no list of uids is refused whole, with nothing stored.
func TestGoneServicesReachThePortal(t *testing.T) {
	r := newRig()
	r.protocol = "2"
	keep := map[string]string{OwnPrefix + KnobKeepAddress: "true"}
	gone := []Gone{{UID: "u-2", Annotations: keep}, {UID: "u-3"}}
	w, ans := r.put(t, "PVEAPIToken=lb@proxcloud!c1=s1", Request{Services: []Service{web("u-1")}, Gone: gone})
	if w.Code != http.StatusOK || len(ans.Services) != 1 || !reflect.DeepEqual(r.portal.gone, gone) {
		t.Fatalf("%d %s: the portal heard %+v", w.Code, w.Body, r.portal.gone)
	}

	// A state at its limit, and fifty Services being deleted on top.
	big := web("u-1")
	big.Annotations = map[string]string{}
	for i := 0; len(mustJSON(Request{Services: []Service{big}})) < maxBody-200; i++ {
		big.Annotations[fmt.Sprintf("example.com/%04d", i)] = strings.Repeat("x", 100)
	}
	var many []Gone
	for i := range MaxGone {
		many = append(many, Gone{UID: fmt.Sprintf("u-gone-%d", i), Annotations: keep})
	}
	if raw := mustJSON(Request{Services: []Service{big}, Gone: many}); len(raw) <= maxBody {
		t.Fatalf("the request is %d bytes: the test wants one over the state's limit", len(raw))
	}
	if w, _ := r.put(t, "PVEAPIToken=lb@proxcloud!c1=s1", Request{Services: []Service{big}, Gone: many}); w.Code != http.StatusOK || len(r.portal.gone) != MaxGone {
		t.Fatalf("a full state with %d Services being deleted: %d %s", MaxGone, w.Code, w.Body)
	}

	calls := r.portal.calls
	for what, body := range map[string]any{
		"one more than a request names": Request{Gone: append(slices.Clone(many), Gone{UID: "u-more"})},
		"a uid that is none":            Request{Gone: []Gone{{UID: "not a uid"}}},
		"a field the gateway knows not": map[string]any{"services": []any{}, "nodes": []any{}, "gone": []any{map[string]any{"uid": "u-2", "keep": true}}},
	} {
		if w, _ := r.put(t, "PVEAPIToken=lb@proxcloud!c1=s1", body); w.Code != http.StatusBadRequest || r.portal.calls != calls {
			t.Errorf("%s: %d %s, %d calls reached the portal", what, w.Code, w.Body, r.portal.calls-calls)
		}
	}

	r.protocol, r.portal.gone = "1", nil
	if w, _ := r.put(t, "PVEAPIToken=lb@proxcloud!c1=s1", Request{Services: []Service{web("u-1")}, Gone: gone}); w.Code != http.StatusOK || r.portal.gone != nil {
		t.Fatalf("protocol 1: %d, the portal heard %+v", w.Code, r.portal.gone)
	}
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
