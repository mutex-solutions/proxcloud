package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newClientKeyAuth builds an Authenticator with a "main" realm and a stub client-key lookup that
// knows a single (pk → secret, sub) triple.
func newClientKeyAuth(pk, secret, sub string) *Authenticator {
	a := New(slog.Default())
	a.SetRealms([]Realm{{Name: "main", IssuerURI: "https://issuer.example/realms/main"}})
	a.SetClientKeyLookup(func(_ *http.Request, id string) (string, string, bool) {
		if id == pk {
			return secret, sub, true
		}
		return "", "", false
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
	if !clientKeyRe.MatchString("pk"+strings.Repeat("0", 32)+".sk"+strings.Repeat("f", 40)) {
		t.Fatal("clientKeyRe failed to match a valid pk.sk")
	}
}

// Before the main (client) realm is discovered, a PAT must fail closed rather than authenticate
// with an empty issuer (which would escape main-realm scoping).
func TestVerifyClientKeyNoMainRealmRejected(t *testing.T) {
	pk := "pk" + strings.Repeat("a", 32)
	sk := "sk" + strings.Repeat("b", 40)
	a := New(slog.Default()) // realms never set → no "main" realm
	a.SetClientKeyLookup(func(_ *http.Request, id string) (string, string, bool) {
		return sk, "user-sub-123", id == pk
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
