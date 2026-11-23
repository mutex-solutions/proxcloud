package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
)

// certPortal is the project side of the gateway for certificates, as the portal's own does it (internal/platform/
// project/cloud_lb_cluster_tls.go, which the integration tests run against a database): it holds certificates by
// the hash it computes itself, asks for the hashes of a request it does not hold, and serves a Service once it holds
// every one of its certificates. also is a hash it asks for on top, which no Service names.
type certPortal struct {
	held    map[string]lbgw.CertificateData
	uploads []lbgw.CertificateData // every certificate that crossed, in order
	puts    int
	got     []lbgw.Service
	also    string
}

func (p *certPortal) LBClusterState(_ context.Context, _ *lbgw.Credential, services []lbgw.Service, _ []lbgw.Node, certs []lbgw.CertificateData) (*lbgw.Answer, error) {
	p.puts, p.got = p.puts+1, services
	for _, c := range certs {
		p.uploads = append(p.uploads, c)
		p.held[lbgw.SecretHash([]byte(c.Certificate), []byte(c.PrivateKey))] = c
	}
	ans := &lbgw.Answer{}
	if p.also != "" {
		ans.NeedCertificates = append(ans.NeedCertificates, p.also)
	}
	for _, s := range services {
		st := lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateReady, Address: "203.0.113.5"}
		if problem := lbgw.CertProblem(&s); problem != "" {
			st = lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateRefused, Reason: lbgw.ReasonInvalidCertificate, Message: problem}
		}
		for _, c := range s.Certificates {
			if _, ok := p.held[c.SHA256]; c.SHA256 != "" && !ok {
				ans.NeedCertificates = append(ans.NeedCertificates, c.SHA256)
				st = lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateBuilding}
			}
		}
		ans.Services = append(ans.Services, st)
	}
	return ans, nil
}

func tlsSecret(crt, key string) fakeSecret {
	return fakeSecret{typ: "kubernetes.io/tls", data: map[string][]byte{"tls.crt": []byte(crt), "tls.key": []byte(key)}}
}

// The controller against the gateway itself (lbgw.Handler over TLS), for a Service that ends TLS with a Secret.
//
// Which Secrets it reads: the ones an owned Service names, by a GET of the name in the Service's own namespace, and
// no other; one of another type is read and neither hashed nor sent. What crosses: the hash each turn, the Secret
// once, when the portal asked for that hash, and again only after it changed; never one the portal asks for that no
// Service names. A Secret that is gone, of another type, without its two keys or forbidden to the controller is said
// on the Service, and the Services next to it are served. Nothing of a Secret is logged or said in an Event.
func TestTLSFromASecret(t *testing.T) {
	ctx := context.Background()
	const keyA, keyB, keyOther = "KEY-A-MARKER", "KEY-B-MARKER", "KEY-OTHER-MARKER"
	f := &fakeTenant{services: map[string]map[string]any{}, secrets: map[string]fakeSecret{
		"web/shop-tls":      tlsSecret("CERT-A", keyA),
		"web/not-named":     tlsSecret("CERT-OTHER", keyOther),
		"web/registry":      {typ: "kubernetes.io/dockerconfigjson", data: map[string][]byte{"tls.crt": []byte("CERT-OTHER"), "tls.key": []byte(keyOther)}},
		"web/half":          {typ: "kubernetes.io/tls", data: map[string][]byte{"tls.crt": []byte("CERT-OTHER")}},
		"web/binary":        tlsSecret("CERT-OTHER\xff\xfe", keyOther),
		"kube-system/admin": tlsSecret("CERT-OTHER", keyOther),
	}}
	f.nodes = []map[string]any{node("w1", "10.0.0.11", nil)}
	f.add("web", "shop", "u-1", "2026-11-23T10:00:00Z", lbSpec(Class, tcp(443, 30443)))
	f.add("web", "plain", "u-2", "2026-11-23T10:00:01Z", lbSpec(Class, tcp(80, 30080)))
	f.add("web", "notours", "u-3", "2026-11-23T10:00:02Z", lbSpec("example.com/other", tcp(443, 30444)))
	secret := lbgw.OwnPrefix + lbgw.KnobTLSSecret
	annotate(f.svc("web", "shop"), secret, "shop-tls")
	annotate(f.svc("web", "notours"), secret, "not-named") // a Service of another class: its Secret is not read

	portal := &certPortal{held: map[string]lbgw.CertificateData{}}
	ts := httptest.NewTLSServer(&lbgw.Handler{Reg: oneCluster{}, Portal: portal, Log: quiet()})
	defer ts.Close()
	var logs bytes.Buffer
	g := &gateway{url: ts.URL, dir: gatewayDir(t, ts)}
	c := &controller{api: f, put: g.put, log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	turn := func() (events []string, puts int, uploads []lbgw.CertificateData) {
		t.Helper()
		f.events, f.reads = nil, nil
		before, sent := portal.puts, len(portal.uploads)
		if err := c.turn(ctx); err != nil {
			t.Fatal(err)
		}
		return reasons(f.events), portal.puts - before, portal.uploads[sent:]
	}
	served := func(name string) bool { return hasIngress(ingress(f.svc("web", name)), "203.0.113.5") }
	sent := func(name string) lbgw.Service {
		t.Helper()
		i := slices.IndexFunc(portal.got, func(s lbgw.Service) bool { return s.Name == name })
		if i < 0 {
			t.Fatalf("Service %s did not reach the portal: %+v", name, portal.got)
		}
		return portal.got[i]
	}
	a := lbgw.CertificateData{Certificate: "CERT-A", PrivateKey: keyA}

	// A new Secret: its hash in the first request, the Secret itself in a second one of the same turn, once.
	events, puts, uploads := turn()
	if puts != 2 || !slices.Equal(uploads, []lbgw.CertificateData{a}) || !served("shop") || !served("plain") ||
		!slices.Equal(f.reads, []string{"web/shop-tls"}) || !slices.Contains(events, "shop:EnsuredLoadBalancer") {
		t.Fatalf("a new Secret: %d requests, %d certificates sent, read %v, events %v", puts, len(uploads), f.reads, events)
	}
	if got := sent("shop").Certificates; !slices.Equal(got, []lbgw.CertRef{{Secret: "shop-tls", SHA256: lbgw.SecretHash([]byte("CERT-A"), []byte(keyA))}}) ||
		sent("plain").Certificates != nil {
		t.Fatalf("what the Services name: %+v", got)
	}
	// Unchanged: one request, the hash alone. The Secret is read again each turn, which is how a change is seen.
	if events, puts, uploads = turn(); puts != 1 || len(uploads) != 0 || len(events) != 0 || !slices.Equal(f.reads, []string{"web/shop-tls"}) {
		t.Fatalf("an unchanged Secret: %d requests, %d certificates sent, events %v", puts, len(uploads), events)
	}
	// Rotated: another hash, and the new Secret crosses once.
	f.secrets["web/shop-tls"] = tlsSecret("CERT-B", keyB)
	if _, puts, uploads = turn(); puts != 2 || !slices.Equal(uploads, []lbgw.CertificateData{{Certificate: "CERT-B", PrivateKey: keyB}}) {
		t.Fatalf("a rotated Secret: %d requests, %d certificates sent", puts, len(uploads))
	}
	// A portal that asks for a hash no Service names gets nothing for it: the controller sends what it read for
	// its Services this turn, and holds nothing else.
	portal.also = lbgw.SecretHash([]byte("CERT-OTHER"), []byte(keyOther))
	if _, puts, uploads = turn(); puts != 1 || len(uploads) != 0 {
		t.Fatalf("a hash no Service names: %d requests, %d certificates sent", puts, len(uploads))
	}
	portal.also = ""
	// A portal that lost what it was sent asks again: it gets the Secret once more, and not at every turn.
	clear(portal.held)
	if _, puts, uploads = turn(); puts != 1 || len(uploads) != 0 {
		t.Fatalf("a Secret asked for again within the minute: %d requests, %d certificates sent", puts, len(uploads))
	}
	c.sent = nil // the minute has passed
	if _, puts, uploads = turn(); puts != 2 || len(uploads) != 1 {
		t.Fatalf("a Secret asked for again after the minute: %d requests, %d certificates sent", puts, len(uploads))
	}

	// A second Service with a Secret of its own: the one the portal lacks crosses, the one it holds does not, however
	// long ago it was sent.
	f.secrets["web/api-tls"] = tlsSecret("CERT-API", "KEY-API-MARKER")
	f.add("web", "api", "u-4", "2026-11-23T10:00:03Z", lbSpec(Class, tcp(443, 30445)))
	annotate(f.svc("web", "api"), secret, "api-tls")
	c.sent = nil
	if _, puts, uploads = turn(); puts != 2 || !slices.Equal(uploads, []lbgw.CertificateData{{Certificate: "CERT-API", PrivateKey: "KEY-API-MARKER"}}) ||
		!slices.Equal(f.reads, []string{"web/shop-tls", "web/api-tls"}) {
		t.Fatalf("a second Service's Secret: %d requests, %d certificates sent, read %v", puts, len(uploads), f.reads)
	}
	delete(f.services, "web/api")

	// What a Service cannot name, and what the controller says of a Secret it cannot use.
	for _, tc := range []struct {
		name, value, problem string
		reads                []string
	}{
		{"a Secret of another namespace", "kube-system/admin", lbgw.CertBadNames, nil},
		{"a path", "../../kube-system/admin", lbgw.CertBadNames, nil},
		{"five Secrets", "a,b,c,d,e", lbgw.CertBadNames, nil},
		{"a Secret that is gone", "gone-tls", lbgw.CertNotFound, []string{"web/gone-tls"}},
		{"a Secret of another type", "registry", lbgw.CertWrongType, []string{"web/registry"}},
		{"a Secret without its key", "half", lbgw.CertIncomplete, []string{"web/half"}},
		{"a Secret that is not text", "binary", lbgw.CertNotText, []string{"web/binary"}},
	} {
		annotate(f.svc("web", "shop"), secret, tc.value)
		c.events = map[string]string{} // an Event that says the same as the last one is not said again
		events, puts, uploads = turn()
		got := sent("shop").Certificates
		if puts != 1 || len(uploads) != 0 || len(got) != 1 || got[0].Problem != tc.problem || got[0].SHA256 != "" || !slices.Equal(f.reads, tc.reads) ||
			!slices.Contains(events, "shop:"+lbgw.ReasonInvalidCertificate) || !served("plain") {
			t.Errorf("%s: %d requests, %d certificates sent, named %+v, read %v, events %v", tc.name, puts, len(uploads), got, f.reads, events)
		}
	}
	// The second of two Secrets is the one that is gone: the first one is named with its hash, and nothing is sent
	// for a Service the portal holds back.
	annotate(f.svc("web", "shop"), secret, "shop-tls, gone-tls")
	if _, _, uploads = turn(); len(sent("shop").Certificates) != 2 || sent("shop").Certificates[0].SHA256 == "" || sent("shop").Certificates[1].Problem != lbgw.CertNotFound || len(uploads) != 0 {
		t.Errorf("two Secrets, one gone: %+v, %d certificates sent", sent("shop").Certificates, len(uploads))
	}

	// A controller whose identity may not read Secrets (a chart that gives it one without "get" on them): the
	// Service says so, and every other Service is served as before.
	annotate(f.svc("web", "shop"), secret, "shop-tls")
	f.forbidden = true
	events, _, uploads = turn()
	if got := sent("shop").Certificates; len(got) != 1 || got[0].Problem != lbgw.CertForbidden || len(uploads) != 0 || !served("plain") ||
		!slices.Contains(events, "shop:"+lbgw.ReasonInvalidCertificate) {
		t.Errorf("a forbidden read: %+v, events %v", got, events)
	}
	var said []string
	for _, ev := range f.events {
		said = append(said, ev["message"].(string))
	}
	if !strings.Contains(strings.Join(said, "\n"), "may not be read by the cluster's load balancer controller") {
		t.Errorf("the Event of a forbidden read: %v", said)
	}
	f.forbidden = false

	// Nothing of a Secret in a log line or an Event.
	for where, text := range map[string]string{"log": logs.String(), "events": strings.Join(said, "\n")} {
		for _, key := range []string{keyA, keyB, keyOther, "CERT-A", "CERT-B", "CERT-OTHER"} {
			if strings.Contains(text, key) {
				t.Errorf("the %s holds %s", where, key)
			}
		}
	}
}

// A portal of protocol 1 (one rolled back to v0.11.0) ends no TLS and reads no annotation: a Service that asks for
// TLS is kept out of the request it gets, so its port is never public without TLS, no certificate is sent, and the
// Service says why it has no address.
func TestTLSServiceAndAPortalOfProtocol1(t *testing.T) {
	ctx := context.Background()
	tls := lbgw.Service{UID: "u-1", Annotations: map[string]string{lbgw.OwnPrefix + lbgw.KnobTLSSecret: "shop-tls"},
		Certificates: []lbgw.CertRef{{Secret: "shop-tls", SHA256: strings.Repeat("a", 64)}}}
	old := protocol1(&lbgw.Request{Services: []lbgw.Service{tls, {UID: "u-2"}},
		CertificateData: []lbgw.CertificateData{{Certificate: "c", PrivateKey: "k"}}})
	if len(old.Services) != 1 || old.Services[0].UID != "u-2" || old.CertificateData != nil {
		t.Fatalf("the request of protocol 1: %+v", old)
	}

	f := &fakeTenant{services: map[string]map[string]any{}, secrets: map[string]fakeSecret{"web/shop-tls": tlsSecret("CERT-A", "KEY-A")}}
	f.add("web", "shop", "u-1", "2026-11-23T10:00:00Z", lbSpec(Class, tcp(443, 30443)))
	annotate(f.svc("web", "shop"), lbgw.OwnPrefix+lbgw.KnobTLSSecret, "shop-tls")
	f.svc("web", "shop")["status"] = map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"ip": "203.0.113.5", "ipMode": "VIP"}}}}
	f.svc("web", "shop")["metadata"].(map[string]any)["finalizers"] = []any{Finalizer}
	// The answer of a portal that got the request of protocol 1: the Service is not in it.
	g := &gatewayFake{}
	c := &controller{api: f, log: quiet(), put: func(_ context.Context, req *lbgw.Request) (*lbgw.Answer, error) {
		return g.put(ctx, protocol1(req))
	}}
	if err := c.turn(ctx); err != nil {
		t.Fatal(err)
	}
	if len(g.last.Services) != 0 || len(ingress(f.svc("web", "shop"))) != 0 || !slices.Equal(reasons(f.events), []string{"shop:" + lbgw.ReasonTLSNotAvailable}) {
		t.Fatalf("sent %+v, ingress %v, events %v", g.last.Services, ingress(f.svc("web", "shop")), reasons(f.events))
	}
}
