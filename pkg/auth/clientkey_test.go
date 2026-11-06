package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newClientKeyAuth builds an Authenticator with a "main" realm and a stub client-key lookup that
// knows a single (pk → secret, sub) triple.
func newClientKeyAuth(pk, secret, sub string) *Authenticator {
	a := New(slog.Default())
	a.SetRealms([]Realm{{Name: "main", IssuerURI: "https://issuer.example/realms/main"}})
	a.SetClientKeyLookup(func(_ *http.Request, id string) (ClientKey, bool) {
		if id == pk {
			return ClientKey{SecretHash: HashClientKeySecret(secret), Sub: sub}, true
		}
		return ClientKey{}, false
	})
	return a
}

func TestVerifyClientKey(t *testing.T) {
	pk := "pk" + strings.Repeat("a", 32)
	sk := "sk" + strings.Repeat("b", 40)
	a := newClientKeyAuth(pk, sk, "user-sub-123")

	// A valid PAT resolves to the owning user's principal, scoped to the main realm issuer.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/details", nil)
	req.Header.Set("Authorization", "Bearer "+pk+"."+sk)
	rc, ok := a.authenticate(req)
	if !ok {
		t.Fatal("valid PAT rejected")
	}
	if rc.Sub != "user-sub-123" {
		t.Fatalf("Sub = %q, want user-sub-123", rc.Sub)
	}
	if rc.Issuer != "https://issuer.example/realms/main" {
		t.Fatalf("Issuer = %q, want main realm", rc.Issuer)
	}
	if a.isAdmin(rc) {
		t.Fatal("client PAT must never be admin")
	}

	// Wrong secret half → reject (constant-time compare miss).
	req.Header.Set("Authorization", "Bearer "+pk+".sk"+strings.Repeat("c", 40))
	if _, ok := a.authenticate(req); ok {
		t.Fatal("PAT with wrong secret accepted")
	}

	// Unknown pk → reject.
	req.Header.Set("Authorization", "Bearer pk"+strings.Repeat("9", 32)+"."+sk)
	if _, ok := a.authenticate(req); ok {
		t.Fatal("unknown PAT accepted")
	}
}

// A real JWT (three dot-separated base64 segments) must NOT match the pk.sk shape — it falls
// through to verifyJWT (which rejects here since no realm verifier is wired), never the PAT path.
func TestClientKeyRegexIsolatesJWT(t *testing.T) {
	if clientKeyRe.MatchString("eyJhbGciOi.eyJzdWIiOi.sIgnAtUrE") {
		t.Fatal("clientKeyRe wrongly matched a JWT")
	}
	if !clientKeyRe.MatchString("pk" + strings.Repeat("0", 32) + ".sk" + strings.Repeat("f", 40)) {
		t.Fatal("clientKeyRe failed to match a valid pk.sk")
	}
}

// Before the main (client) realm is discovered, a PAT must fail closed rather than authenticate
// with an empty issuer (which would escape main-realm scoping).
func TestVerifyClientKeyNoMainRealmRejected(t *testing.T) {
	pk := "pk" + strings.Repeat("a", 32)
	sk := "sk" + strings.Repeat("b", 40)
	a := New(slog.Default()) // realms never set → no "main" realm
	a.SetClientKeyLookup(func(_ *http.Request, id string) (ClientKey, bool) {
		return ClientKey{SecretHash: HashClientKeySecret(sk), Sub: "user-sub-123"}, id == pk
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/details", nil)
	req.Header.Set("Authorization", "Bearer "+pk+"."+sk)
	if _, ok := a.authenticate(req); ok {
		t.Fatal("PAT accepted with no main realm configured")
	}
}

// An empty sub from the lookup (orphaned key) must be rejected even if the secret matches.
func TestVerifyClientKeyEmptySubRejected(t *testing.T) {
	pk := "pk" + strings.Repeat("d", 32)
	sk := "sk" + strings.Repeat("e", 40)
	a := newClientKeyAuth(pk, sk, "") // lookup returns empty sub
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/details", nil)
	req.Header.Set("Authorization", "Bearer "+pk+"."+sk)
	if _, ok := a.authenticate(req); ok {
		t.Fatal("PAT with empty sub accepted")
	}
}

// What the store holds cannot be presented as the token.
func TestStoredHashIsNotTheCredential(t *testing.T) {
	pk := "pk" + strings.Repeat("a", 32)
	sk := "sk" + strings.Repeat("b", 40)
	a := newClientKeyAuth(pk, sk, "u")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/account/details", nil)
	req.Header.Set("Authorization", "Bearer "+pk+"."+sk)
	if _, ok := a.authenticate(req); !ok {
		t.Fatal("the real secret must authenticate")
	}
	if h := HashClientKeySecret(sk); h == sk || len(h) != 64 {
		t.Fatalf("hash = %q", h)
	}
	a.SetClientKeyLookup(func(*http.Request, string) (ClientKey, bool) {
		return ClientKey{SecretHash: sk, Sub: "u"}, true // a row still holding the plain secret
	})
	if _, ok := a.authenticate(req); ok {
		t.Fatal("a row with a plain secret must not authenticate")
	}
}

func TestVerifyClientKeyScopeAndExpiry(t *testing.T) {
	pk := "pk" + strings.Repeat("a", 32)
	sk := "sk" + strings.Repeat("b", 40)
	withKey := func(k ClientKey) *Authenticator {
		a := newClientKeyAuth(pk, sk, "u")
		k.SecretHash, k.Sub = HashClientKeySecret(sk), "u"
		a.SetClientKeyLookup(func(*http.Request, string) (ClientKey, bool) { return k, true })
		return a
	}
	try := func(a *Authenticator, method, path string) bool {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+pk+"."+sk)
		_, ok := a.authenticate(req)
		return ok
	}
	full := withKey(ClientKey{})
	if !try(full, http.MethodPost, "/api/v1/project/p/cloud") {
		t.Fatal("a token without a scope is a full token")
	}
	read := withKey(ClientKey{Scope: PATScopeRead})
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/api/v1/account/details", true},
		{http.MethodPost, "/api/v1/project/p/resource", true},
		{http.MethodPost, "/api/v1/project/p/cloud", false},
		{http.MethodDelete, "/api/v1/project/p/cloud/x", false},
		{http.MethodPost, "/mcp", false},
		// a write whose path merely ends the same way as the list route
		{http.MethodPost, "/api/v1/user/custom-info/resource", false},
		{http.MethodPost, "/api/v1/project/p/x/resource", false},
		// a read that returns a live credential
		{http.MethodGet, "/api/v1/project/p/s3-credentials", false},
		{http.MethodGet, "/api/v1/project/p/s3-keys", false},
		{http.MethodHead, "/api/v1/project/p/s3-credentials", false},
		{http.MethodGet, "/api/v1/project/p/cost-info", true},
	} {
		if got := try(read, c.method, c.path); got != c.want {
			t.Fatalf("read token %s %s = %v, want %v", c.method, c.path, got, c.want)
		}
	}
	if try(withKey(ClientKey{ExpiresAt: time.Now().Add(-time.Minute)}), http.MethodGet, "/api/v1/account/details") {
		t.Fatal("an expired token must not authenticate")
	}
	if !try(withKey(ClientKey{ExpiresAt: time.Now().Add(time.Hour)}), http.MethodGet, "/api/v1/account/details") {
		t.Fatal("a token before its expiry authenticates")
	}
}
