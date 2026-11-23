package lbgw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func hashOf(i int) string { return fmt.Sprintf("%064x", i+1) }

// A Secret's hash names its certificate and its key together: either one changed is another hash, and the two
// cannot be cut differently into the same hash.
func TestSecretHash(t *testing.T) {
	base := SecretHash([]byte("cert"), []byte("key"))
	if !sha256Re.MatchString(base) || base != SecretHash([]byte("cert"), []byte("key")) {
		t.Fatalf("hash %q", base)
	}
	for name, other := range map[string]string{
		"another key":                  SecretHash([]byte("cert"), []byte("key2")),
		"another certificate":          SecretHash([]byte("cert2"), []byte("key")),
		"the same bytes cut elsewhere": SecretHash([]byte("certk"), []byte("ey")),
	} {
		if other == base {
			t.Errorf("%s gives the same hash", name)
		}
	}
}

// proxcloud.io/lb-tls-secret names Secrets by name alone: nothing in it can point out of the Service's namespace.
func TestTLSSecretNames(t *testing.T) {
	for value, want := range map[string][]string{
		"web-tls":                       {"web-tls"},
		" web-tls , api.tls ":           {"web-tls", "api.tls"},
		"a,b,c,d":                       {"a", "b", "c", "d"},
		"a,b,c,d,e":                     nil, // more than a Service names
		"kube-system/admin":             nil, // a namespace is no part of a name
		"../web-tls":                    nil,
		"web-tls,":                      nil,
		"Web-TLS":                       nil,
		"web-tls,web-tls":               nil,
		"web tls":                       nil,
		strings.Repeat("a", 300):        nil,
		"a," + strings.Repeat("b", 254): nil,
	} {
		got, ok := TLSSecretNames(value)
		if ok != (want != nil) || (ok && !slices.Equal(got, want)) {
			t.Errorf("%q: %v %v, want %v", value, got, ok, want)
		}
	}
}

// The two annotations the portal reads of a Service that ends TLS, and that none of the three is taken for an
// annotation the balancer does not know.
func TestTLSAnnotations(t *testing.T) {
	svc := func(kv ...string) *Service {
		s := annotated(kv...)
		s.Ports = []Port{{Protocol: "TCP", Port: 80, NodePort: 30080}, {Protocol: "TCP", Port: 443, NodePort: 30443}, {Protocol: "UDP", Port: 443, NodePort: 30443}}
		s.Certificates = []CertRef{{Secret: "web-tls", SHA256: hashOf(0)}}
		return s
	}
	ends := func(s *Service, o Options) (out []string) {
		for _, p := range s.Ports {
			if o.EndsTLS(s, p) {
				out = append(out, fmt.Sprintf("%s/%d", p.Protocol, p.Port))
			}
		}
		return out
	}
	secret := OwnPrefix + KnobTLSSecret
	for _, tc := range []struct {
		name    string
		kv      []string
		ends    []string
		tcp     bool
		invalid string
	}{
		{"every TCP port by default, never a UDP port", []string{secret, "web-tls"}, []string{"TCP/80", "TCP/443"}, false, ""},
		{"the ports named", []string{secret, "web-tls", OwnPrefix + KnobTLSPorts, " 443 "}, []string{"TCP/443"}, false, ""},
		{"plain TCP to the members", []string{secret, "web-tls", OwnPrefix + KnobBackendProtocol, "TCP"}, []string{"TCP/80", "TCP/443"}, true, ""},
		{"http said aloud", []string{secret, "web-tls", OwnPrefix + KnobBackendProtocol, "http"}, []string{"TCP/80", "TCP/443"}, false, ""},
		// A list that cannot be read leaves no port plain that was to end TLS: the default holds.
		{"a port the Service does not have", []string{secret, "web-tls", OwnPrefix + KnobTLSPorts, "443,8443"}, []string{"TCP/80", "TCP/443"}, false, KnobTLSPorts},
		{"not a number", []string{secret, "web-tls", OwnPrefix + KnobTLSPorts, "https"}, []string{"TCP/80", "TCP/443"}, false, KnobTLSPorts},
		{"a backend protocol the balancer has none of", []string{secret, "web-tls", OwnPrefix + KnobBackendProtocol, "grpc"}, []string{"TCP/80", "TCP/443"}, false, KnobBackendProtocol},
	} {
		s := svc(tc.kv...)
		o, reason, msg := ServiceOptions(s)
		if reason != "" {
			t.Fatalf("%s: refused %s %s", tc.name, reason, msg)
		}
		if got := ends(s, o); !slices.Equal(got, tc.ends) || o.TLSBackendTCP != tc.tcp {
			t.Errorf("%s: ends TLS on %v (tcp %v), want %v (tcp %v)", tc.name, got, o.TLSBackendTCP, tc.ends, tc.tcp)
		}
		var said string
		for _, ev := range o.Events {
			if ev.Reason == ReasonAnnotationNotSupported {
				t.Errorf("%s: a TLS annotation is taken for one the balancer does not know: %s", tc.name, ev.Message)
			}
			if ev.Reason == ReasonInvalidAnnotation {
				said = ev.Message
			}
		}
		if (tc.invalid == "") != (said == "") || !strings.Contains(said, tc.invalid) {
			t.Errorf("%s: InvalidAnnotation %q, want one naming %q", tc.name, said, tc.invalid)
		}
	}
	// Without its certificates no port ends TLS, whatever the annotations say: the portal then holds the Service
	// back (CertProblem), it does not serve it plain.
	s := svc(secret, "web-tls")
	s.Certificates = nil
	if o, _, _ := ServiceOptions(s); len(ends(s, o)) != 0 || CertProblem(s) == "" || !WantsTLS(s) {
		t.Errorf("a Service without its certificates: ends %v, problem %q", ends(s, o), CertProblem(s))
	}
	if plain := annotated(); WantsTLS(plain) || CertProblem(plain) != "" {
		t.Error("a Service without the annotation asks for TLS")
	}
}

// What a Service's owner reads for each reason a Secret cannot be used: the Secret by namespace and name, and
// nothing a controller wrote.
func TestCertProblem(t *testing.T) {
	for code, want := range map[string]string{
		CertNotFound: "Secret shop/web-tls does not exist", CertWrongType: "is not of type kubernetes.io/tls",
		CertIncomplete: "tls.crt and tls.key", CertTooLarge: "more than 16 KiB", CertForbidden: "may not be read",
		CertUnreadable: "could not be read", "<script>": "could not be read", CertBadNames: "by name, between commas",
	} {
		s := &Service{Namespace: "shop", Certificates: []CertRef{{Secret: "ok", SHA256: hashOf(0)}, {Secret: "web-tls", Problem: code}}}
		if code == CertBadNames {
			s.Certificates = []CertRef{{Problem: code}}
		}
		if got := CertProblem(s); !strings.Contains(got, want) || strings.Contains(got, "<script>") {
			t.Errorf("%s: %q, want it to hold %q", code, got, want)
		}
	}
}

// Certificates through the gateway: the hashes reach the portal with their Services, what the portal asks for goes
// back, and the Secrets that were asked for reach it. The state's part of the body keeps its 64 KiB, the certificates
// come on top; and nothing of a body that holds a key comes back in an error, a log line or an audit event.
func TestCertificatesThroughTheGateway(t *testing.T) {
	r := newRig()
	r.protocol = "2"
	var logs bytes.Buffer
	r.h.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	auth := "PVEAPIToken=lb@proxcloud!c1=s1"
	tls := func(uid string, hashes ...int) Service {
		s := web(uid, Port{Protocol: "TCP", Port: 443, NodePort: 30443})
		s.Annotations = map[string]string{OwnPrefix + KnobTLSSecret: "web-tls"}
		for _, h := range hashes {
			s.Certificates = append(s.Certificates, CertRef{Secret: fmt.Sprintf("tls-%d", h), SHA256: hashOf(h)})
		}
		return s
	}
	const marker = "MARKER-OF-A-KEY-LINE"
	pemKey := "-----BEGIN PRIVATE KEY-----\n" + marker + "\n-----END PRIVATE KEY-----\n"
	data := CertificateData{Certificate: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", PrivateKey: pemKey}

	// The first request names a hash; the portal asks for it.
	r.portal.need = []string{hashOf(0)}
	w, ans := r.put(t, auth, Request{Services: []Service{tls("u1", 0)}})
	if w.Code != http.StatusOK || !slices.Equal(ans.NeedCertificates, []string{hashOf(0)}) || len(r.portal.certs) != 0 ||
		len(r.portal.got) != 1 || !slices.Equal(r.portal.got[0].Certificates, []CertRef{{Secret: "tls-0", SHA256: hashOf(0)}}) {
		t.Fatalf("the first request: %d %+v, the portal got %+v and %d certificates", w.Code, ans, r.portal.got, len(r.portal.certs))
	}
	// The next one carries the Secret.
	r.portal.need = nil
	w, ans = r.put(t, auth, Request{Services: []Service{tls("u1", 0)}, CertificateData: []CertificateData{data}})
	if w.Code != http.StatusOK || len(ans.NeedCertificates) != 0 || len(r.portal.certs) != 1 || r.portal.certs[0] != data {
		t.Fatalf("the request with the Secret: %d, the portal got %d certificates", w.Code, len(r.portal.certs))
	}

	// The size of the body. Sixteen certificates of 16 KiB each pass next to a state; a state over 64 KiB is refused
	// with or without them, and so are seventeen certificates.
	line := func(c string) string { return strings.Repeat(strings.Repeat(c, 63)+"\n", 128) } // 8 KiB of PEM lines
	full := CertificateData{Certificate: line("c"), PrivateKey: line("k")}
	sixteen := slices.Repeat([]CertificateData{full}, MaxClusterCerts)
	if w, _ := r.put(t, auth, Request{Services: []Service{tls("u1", 0)}, CertificateData: sixteen}); w.Code != http.StatusOK || len(r.portal.certs) != MaxClusterCerts {
		t.Errorf("sixteen certificates of 16 KiB: %d %s", w.Code, w.Body)
	}
	calls := r.portal.calls
	fat := tls("u1", 0)
	fat.SourceRanges = slices.Repeat([]string{"198.51.100.0/24"}, 5000)
	for name, body := range map[string]Request{"alone": {Services: []Service{fat}}, "with a certificate": {Services: []Service{fat}, CertificateData: []CertificateData{data}}} {
		if w, _ := r.put(t, auth, body); w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("a state over 64 KiB %s: %d", name, w.Code)
		}
	}
	if w, _ := r.put(t, auth, Request{Services: []Service{tls("u1", 0)}, CertificateData: slices.Repeat([]CertificateData{data}, MaxClusterCerts+1)}); w.Code != http.StatusBadRequest {
		t.Errorf("seventeen certificates: %d", w.Code)
	}
	// A body that is no state and holds a key: the answer names nothing of it.
	for _, raw := range []string{
		`{"services":[],"nodes":[],"certificateData":[{"certificate":"x","privateKey":"` + marker + `","more":1}]}`,
		`{"services":[],"nodes":[],"certificateData":[{"certificate":"x","privateKey":"` + marker + `"}],"` + marker + `":1}`,
		`{"services":[],"certificateData":[{"privateKey":"-----BEGIN PRIVATE KEY-----` + marker,
	} {
		req := httptest.NewRequest(http.MethodPut, StatePath, strings.NewReader(raw))
		req.Header.Set("Authorization", auth)
		req.Header.Set(ProtocolHeader, "2")
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), marker) {
			t.Errorf("a broken body with a key: %d %s", rec.Code, rec.Body)
		}
	}
	if r.portal.calls != calls {
		t.Errorf("a refused body reached the portal")
	}

	// The form of a Service's certificates: a refusal of that Service alone.
	for name, refs := range map[string][]CertRef{
		"five Secrets":            {{Secret: "a", SHA256: hashOf(0)}, {Secret: "b", SHA256: hashOf(1)}, {Secret: "c", SHA256: hashOf(2)}, {Secret: "d", SHA256: hashOf(3)}, {Secret: "e", SHA256: hashOf(4)}},
		"no hash":                 {{Secret: "a"}},
		"a hash that is none":     {{Secret: "a", SHA256: "../../etc/passwd"}},
		"a name with a namespace": {{Secret: "kube-system/a", SHA256: hashOf(0)}},
		"a problem and a hash":    {{Secret: "a", SHA256: hashOf(0), Problem: CertNotFound}},
		"a problem of many words": {{Secret: "a", Problem: strings.Repeat("x", 40)}},
	} {
		s := tls("u9")
		s.Certificates = refs
		_, ans := r.put(t, auth, Request{Services: []Service{tls("u1", 0), s}})
		if len(ans.Services) != 2 || ans.Services[0].State == StateRefused || ans.Services[1].Reason != ReasonInvalid {
			t.Errorf("%s: %+v", name, ans.Services)
		}
	}
	// A Secret that cannot be used is the portal's to answer: it knows what the balancer serves.
	gone := tls("u2")
	gone.Certificates = []CertRef{{Secret: "web-tls", Problem: CertNotFound}}
	if _, ans := r.put(t, auth, Request{Services: []Service{gone}}); ans.Services[0].State == StateRefused || len(r.portal.got) != 1 || r.portal.got[0].Certificates[0].Problem != CertNotFound {
		t.Errorf("a Secret that is gone: %+v, the portal got %+v", ans.Services, r.portal.got)
	}

	// Sixteen certificates a cluster: the Service that names the seventeenth is refused, the ones before and after
	// it that fit are not.
	var many []Service
	for i := 0; i < 5; i++ {
		many = append(many, tls(fmt.Sprintf("m%d", i), 4*i, 4*i+1, 4*i+2, 4*i+3))
	}
	many = append(many, tls("again", 0, 1)) // certificates the cluster already has
	_, ans = r.put(t, auth, Request{Services: many})
	for i, st := range ans.Services {
		if want := i == 4; (st.Reason == ReasonTooLarge) != want {
			t.Errorf("Service %d of a cluster with too many certificates: %+v", i, st)
		}
	}
	if len(r.portal.got) != 5 {
		t.Errorf("the portal got %d Services", len(r.portal.got))
	}

	// A controller of protocol 1 sends no certificates, and none is read for it.
	r.protocol = "1"
	if _, _ = r.put(t, auth, Request{Services: []Service{tls("u1", 0)}}); len(r.portal.got) != 1 || r.portal.got[0].Certificates != nil {
		t.Errorf("protocol 1: the portal got %+v", r.portal.got)
	}

	// Nothing the gateway wrote holds a key, and a certificate prints as nothing whatever prints it.
	audited, _ := json.Marshal(r.events)
	printed := fmt.Sprintf("%v %+v %#v %s", data, data, data, []CertificateData{data})
	slog.New(slog.NewTextHandler(&logs, nil)).Info("a certificate", "data", data, "list", []CertificateData{data})
	for where, text := range map[string]string{"log": logs.String(), "audit": string(audited), "printed": printed} {
		if strings.Contains(text, marker) || strings.Contains(text, "PRIVATE KEY") {
			t.Errorf("the %s holds a key: %s", where, text)
		}
	}
}
