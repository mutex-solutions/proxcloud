package lbgw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

func (f *fakeReg) Seen(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen++
	f.creds[id].LastSeenAt = &at
	return nil
}

// fakePortal accepts every Service as building and records what reached it.
type fakePortal struct {
	got     []Service
	nodes   []Node
	refusal *Refusal
	calls   int
}

func (p *fakePortal) LBClusterState(_ context.Context, cred *Credential, services []Service, nodes []Node) (*Answer, error) {
	p.calls++
	if p.refusal != nil {
		return nil, p.refusal
	}
	p.got, p.nodes = services, nodes
	ans := &Answer{}
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
	req.Header.Set(ProtocolHeader, "1")
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
	if w.Code != http.StatusOK || len(ans.Services) != 1 || ans.Services[0].State != StateBuilding || w.Header().Get(ProtocolHeader) != "1" {
		t.Fatalf("own credential: %d %s", w.Code, w.Body)
	}
	// The last call is written at most once a minute.
	r.put(t, "PVEAPIToken lb@proxcloud!c1=s1", body)
	if r.reg.seen != 1 {
		t.Fatalf("last seen written %d times", r.reg.seen)
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
	want := map[string]string{"ok": "", "udp": ReasonUDPNotSupported, "mixed": ReasonUDPNotSupported, "sctp": ReasonUDPNotSupported,
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
	for name, tc := range map[string]struct {
		method, path, proto string
		body                []byte
		status              int
		reason              string
	}{
		"other path":      {http.MethodPut, "/lb/v1/other", "1", ok, http.StatusNotFound, ""},
		"query":           {http.MethodPut, StatePath + "?project=p2", "1", ok, http.StatusNotFound, ""},
		"get":             {http.MethodGet, StatePath, "1", nil, http.StatusMethodNotAllowed, ""},
		"no protocol":     {http.MethodPut, StatePath, "", ok, http.StatusConflict, ReasonControllerTooOld},
		"old protocol":    {http.MethodPut, StatePath, "0", ok, http.StatusConflict, ReasonControllerTooOld},
		"too large":       {http.MethodPut, StatePath, "1", big, http.StatusRequestEntityTooLarge, ReasonTooLarge},
		"unknown field":   {http.MethodPut, StatePath, "1", []byte(`{"services":[],"projectId":"p2"}`), http.StatusBadRequest, ReasonInvalid},
		"bad node":        {http.MethodPut, StatePath, "1", []byte(`{"nodes":[{"address":"fd00::1"}]}`), http.StatusBadRequest, ReasonInvalid},
		"duplicate uid":   {http.MethodPut, StatePath, "1", []byte(`{"services":[{"uid":"a","ports":[{"port":80,"nodePort":30080}]},{"uid":"a","ports":[{"port":81,"nodePort":30081}]}]}`), http.StatusBadRequest, ReasonInvalid},
		"name not a name": {http.MethodPut, StatePath, "1", []byte(`{"services":[{"uid":"a","name":"x\ny","ports":[{"port":80,"nodePort":30080}]}]}`), http.StatusBadRequest, ReasonInvalid},
	} {
		w := do(tc.method, tc.path, tc.proto, tc.body)
		var got map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != tc.status || got["reason"] != tc.reason {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if r.portal.calls != 0 || len(r.events) != 10 {
		t.Fatalf("portal reached %d times, %d audit events", r.portal.calls, len(r.events))
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
