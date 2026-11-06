package sso

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	for raw, want := range map[string]string{" Example.COM. ": "example.com", "a-b.co.uk": "a-b.co.uk"} {
		if got, err := normalizeDomain(raw); err != nil || got != want {
			t.Errorf("normalizeDomain(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "com", "not a domain", "-a.com", "a..com", "a@b.com", "http://a.com", "a.c", strings.Repeat("a.", 130) + "com"} {
		if got, err := normalizeDomain(raw); err == nil {
			t.Errorf("normalizeDomain(%q) accepted as %q", raw, got)
		}
	}
}

func TestIsReservedCoversSubdomainsAndParents(t *testing.T) {
	s := &Service{reserved: []string{"ops.example.com"}}
	for domain, want := range map[string]bool{
		"ops.example.com":      true,
		"eu.ops.example.com":   true,
		"example.com":          true, // a parent could assert addresses under the reserved one
		"notops.example.com":   false,
		"ops.example.com.evil": false,
		"other.test":           false,
	} {
		if got := s.isReserved(domain); got != want {
			t.Errorf("isReserved(%q) = %v", domain, got)
		}
	}
}

func TestReservedFromURLs(t *testing.T) {
	got := ReservedFromURLs("https://app.example.com/x", "https://admin.example.com", "https://example.org", "http://10.0.0.1:8080", "http://localhost", "::bad")
	if len(got) != 2 || got[0] != "example.com" || got[1] != "example.org" {
		t.Fatalf("got %v", got)
	}
}

func TestPickRoleSortedFirstMatchThenDefault(t *testing.T) {
	p := &Provider{DefaultRole: "MEMBER", GroupRoleMap: map[string]string{"zeta": "Z", "alpha": "A"}}
	if r := pickRole(p, []string{"zeta", "alpha", "other"}); r != "A" {
		t.Fatalf("got %s", r)
	}
	if r := pickRole(p, []string{"other"}); r != "MEMBER" {
		t.Fatalf("got %s", r)
	}
	if r := pickRole(p, nil); r != "MEMBER" {
		t.Fatalf("got %s", r)
	}
}

func TestDomainOf(t *testing.T) {
	for in, want := range map[string]string{"a@B.COM": "b.com", "a@x@b.com": "b.com", "nope": "", "": ""} {
		if got := domainOf(in); got != want {
			t.Errorf("domainOf(%q) = %q", in, got)
		}
	}
}

func TestBlockedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1", "fe80::1", "::ffff:10.0.0.1", "224.0.0.1"} {
		if !blockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if blockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

// The guarded fetcher must not reach the management network, directly or through a redirect.
func TestFetcherRefusesInternalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"x"}`))
	}))
	defer srv.Close()
	ctx := context.Background()

	strict := newFetcher(false)
	if _, err := strict.get(ctx, srv.URL); err == nil {
		t.Fatal("plain http to loopback accepted")
	}
	// https scheme but an internal address: refused at connect time.
	if _, err := strict.get(ctx, "https://"+srv.Listener.Addr().String()+"/"); err == nil {
		t.Fatal("https to loopback accepted")
	}
	if err := strict.checkURL(ctx, "https://127.0.0.1/token"); err == nil {
		t.Fatal("checkURL accepted loopback")
	}
	if err := strict.checkURL(ctx, "https://user:pw@example.com/"); err == nil {
		t.Fatal("checkURL accepted credentials in the URL")
	}

	dev := newFetcher(true)
	body, err := dev.get(ctx, srv.URL)
	if err != nil || !strings.Contains(string(body), "issuer") {
		t.Fatalf("development fetch: %v %s", err, body)
	}
	if _, err := dev.get(ctx, "ftp://x.test/"); err == nil {
		t.Fatal("ftp accepted")
	}
}

func TestFetcherLimitsDocumentSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxMetadataBytes+10))
	}))
	defer srv.Close()
	if _, err := newFetcher(true).get(context.Background(), srv.URL); err == nil {
		t.Fatal("oversized document accepted")
	}
}

func TestRepresentationNeverAutoLinks(t *testing.T) {
	p := &Provider{Alias: "org-1", Protocol: protoOIDC, ClientID: "c", ProviderName: "N", Scopes: defaultScopes, Enabled: true, TrustEmail: true}
	rep := representation(p, map[string]string{"tokenUrl": "https://x/t"}, "s3cret")
	if rep["firstBrokerLoginFlowAlias"] != "first broker login" || rep["hideOnLogin"] != true || rep["enabled"] != true {
		t.Fatalf("rep: %v", rep)
	}
	cfg := rep["config"].(map[string]string)
	if cfg["clientSecret"] != "s3cret" || cfg["syncMode"] != "FORCE" || cfg["validateSignature"] != "true" || cfg["tokenUrl"] != "https://x/t" {
		t.Fatalf("cfg: %v", cfg)
	}
	p.OperatorDisabled = true
	if representation(p, nil, "s")["enabled"] != false {
		t.Fatal("an operator-disabled provider must be off in Keycloak")
	}
}

func TestNewKCAdminDerivesRealm(t *testing.T) {
	k, err := NewKCAdmin("https://id.example.com/realms/acme", "http://kc:8080/realms/acme/", "c", "s")
	if err != nil || k.base != "http://kc:8080" || k.realm != "acme" {
		t.Fatalf("%+v %v", k, err)
	}
	if _, err := NewKCAdmin("https://id.example.com/", "", "c", "s"); err == nil {
		t.Fatal("issuer without a realm accepted")
	}
}

func TestClientIPTakesLastHop(t *testing.T) {
	r := &http.Request{RemoteAddr: "10.0.0.9:1234", Header: http.Header{}}
	if ip := clientIP(r); ip != "10.0.0.9" {
		t.Fatal(ip)
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 9.9.9.9")
	if ip := clientIP(r); ip != "9.9.9.9" {
		t.Fatal(ip)
	}
}

func TestRateLimiter(t *testing.T) {
	l := &rateLimiter{limit: 2, hits: map[string]*window{}}
	if !l.allow("a") || !l.allow("a") || l.allow("a") {
		t.Fatal("limit not applied")
	}
	if !l.allow("b") {
		t.Fatal("keys must be independent")
	}
}

func TestImportConfigIsMultipart(t *testing.T) {
	var gotProvider string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":300}`))
			return
		}
		gotProvider = r.FormValue("providerId")
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("no file part: %v", err)
			return
		}
		defer f.Close()
		_, _ = w.Write([]byte(`{"tokenUrl":"https://x/t"}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	k, err := NewKCAdmin("http://"+u.Host+"/realms/r", "", "c", "s")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := k.ImportConfig(context.Background(), "oidc", []byte(`{}`))
	if err != nil || cfg["tokenUrl"] != "https://x/t" || gotProvider != "oidc" {
		t.Fatalf("%v %v %s", cfg, err, gotProvider)
	}
}
