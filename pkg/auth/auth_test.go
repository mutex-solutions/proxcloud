package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestJobPrefixNotPublic pins that operator job triggers are no longer in the permitAll set (anon
// must not be able to drive billing/collect/sync).
func TestJobPrefixNotPublic(t *testing.T) {
	if IsPublic("/api/v1/admin/job/run-collect") {
		t.Error("/api/v1/admin/job/ must NOT be public")
	}
	// SSE is likewise no longer public (finding [23]).
	if IsPublic("/api/v1/events/proj1") {
		t.Error("/api/v1/events/ must NOT be public")
	}
	// Legitimate public paths still pass.
	if !IsPublic("/api/v1/notifications/svc/RegionOne") {
		t.Error("/api/v1/notifications/ should remain public")
	}
}

// TestIsAdmin pins the admin-credential rule that gates the job prefix: SigV4 or an admin/admin-api
// realm bearer is admin; a plain client-realm user is NOT.
func TestIsAdmin(t *testing.T) {
	a := New(slog.Default())
	a.SetRealms([]Realm{
		{Name: "main", IssuerURI: "https://idp/realms/clients", ClientID: "proxcloud-ui"},
		{Name: "admin", IssuerURI: "https://idp/realms/admin", ClientID: "proxcloud-admin"},
		{Name: "admin-api", IssuerURI: "https://idp/realms/admin-api", ClientID: "proxcloud-admin-api"},
	})
	cases := []struct {
		name string
		rc   *httpx.RequestContext
		want bool
	}{
		{"nil", nil, false},
		{"sigv4", &httpx.RequestContext{SigV4KeyID: "pkX"}, true},
		{"admin realm", &httpx.RequestContext{Issuer: "https://idp/realms/admin"}, true},
		{"admin-api realm", &httpx.RequestContext{Issuer: "https://idp/realms/admin-api"}, true},
		{"client realm", &httpx.RequestContext{Issuer: "https://idp/realms/clients", Sub: "u1"}, false},
		{"unknown issuer", &httpx.RequestContext{Issuer: "https://evil/realms/admin"}, false},
	}
	for _, c := range cases {
		if got := a.isAdmin(c.rc); got != c.want {
			t.Errorf("%s: isAdmin = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestEnforceJobGate drives the gate end to end: anon is 401 (job triggers are no longer public);
// a valid SigV4 admin credential passes.
func TestEnforceJobGate(t *testing.T) {
	a := New(slog.Default())
	a.SetHmacLookup(func(_ context.Context, id string) (string, bool) {
		if id == "pkADMIN" {
			return "skADMIN", true
		}
		return "", false
	})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	// Anonymous → 401 (no longer in publicPrefix).
	rec := httptest.NewRecorder()
	a.Enforce(next).ServeHTTP(rec, httptest.NewRequest("POST", "http://api.test/api/v1/admin/job/run-collect", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: want 401, got %d", rec.Code)
	}

	// Valid SigV4 admin key → isAdmin → passes to next.
	amzDate := time.Now().UTC().Format("20060102T150405Z")
	authz := signV4("POST", "/api/v1/admin/job/run-collect", "", "api.test", amzDate, "pkADMIN", "skADMIN", "us-east-1", "execute-api", nil)
	req := httptest.NewRequest("POST", "http://api.test/api/v1/admin/job/run-collect", nil)
	req.Host = "api.test"
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("Authorization", authz)
	rec2 := httptest.NewRecorder()
	a.Enforce(next).ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNoContent {
		t.Errorf("sigv4 admin: want 204, got %d", rec2.Code)
	}
}

// TestAzpAllowed pins the authorized-party binding: a token minted for a different client in the
// same issuer is rejected; a matching or absent azp is accepted.
func TestAzpAllowed(t *testing.T) {
	cases := []struct {
		realmClientID, azp string
		want               bool
	}{
		{"proxcloud-ui", "proxcloud-ui", true},  // match
		{"proxcloud-ui", "", true},              // azp absent → allowed (Keycloak stamps azp on client tokens)
		{"proxcloud-ui", "attacker-cli", false}, // cross-client token → rejected
		{"", "anything", true},                  // no configured client id → nothing to bind
	}
	for _, c := range cases {
		if got := azpAllowed(c.realmClientID, c.azp); got != c.want {
			t.Errorf("azpAllowed(%q,%q) = %v, want %v", c.realmClientID, c.azp, got, c.want)
		}
	}
}

// TestClaimTrue pins email_verified parsing: a JSON bool or the string "true"
// counts; absent, null, "false" or any other shape fails closed.
func TestClaimTrue(t *testing.T) {
	cases := []struct {
		claims string
		want   bool
	}{
		{`{"email_verified":true}`, true},
		{`{"email_verified":"true"}`, true},
		{`{"email_verified":"TRUE"}`, true},
		{`{"email_verified":false}`, false},
		{`{"email_verified":"false"}`, false},
		{`{"email_verified":null}`, false},
		{`{"email_verified":1}`, false},
		{`{}`, false},
	}
	for _, c := range cases {
		var claims struct {
			EmailVerified any `json:"email_verified"`
		}
		if err := json.Unmarshal([]byte(c.claims), &claims); err != nil {
			t.Fatal(err)
		}
		if got := claimTrue(claims.EmailVerified); got != c.want {
			t.Errorf("claimTrue(%s) = %v, want %v", c.claims, got, c.want)
		}
	}
}
