// Package auth is the Resource Server enforcement layer (Keycloak is
// the IdP; ProxCloud only validates tokens). Enforcement:
//   - a whitelist of permitAll paths passes through,
//   - every other /api/v1/** and /admin-api/v1/** request must carry a valid
//     credential, else 401 with an empty body + `WWW-Authenticate: Bearer`.
//
// Token *validation* uses the per-realm OIDC verifiers (Keycloak). When the
// issuer is unreachable (verifier nil) or the token is absent/invalid, the
// request is rejected 401.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Realm is a validated issuer (built from config + discovery).
type Realm struct {
	Name      string
	ClientID  string
	IssuerURI string
	Verifier  *oidc.IDTokenVerifier // nil if discovery hasn't succeeded
}

type Authenticator struct {
	log             *slog.Logger
	mu              sync.RWMutex
	realms          []Realm
	hmacLookup      HmacKeyLookup   // hmac_keys resolver for SigV4 (Admin API) — see sigv4.go
	clientKeyLookup ClientKeyLookup // client_api_keys resolver for `Bearer pk.sk` PATs — see clientkey.go
	guard           GuardFunc
}

// GuardFunc gets the last word on an authenticated request; a non-empty result is the refusal code.
type GuardFunc func(ctx context.Context, rc *httpx.RequestContext) string

// SetGuard installs the guard (per-organization SSO rules). nil means none.
func (a *Authenticator) SetGuard(g GuardFunc) {
	a.mu.Lock()
	a.guard = g
	a.mu.Unlock()
}

func (a *Authenticator) refusal(r *http.Request, rc *httpx.RequestContext) string {
	a.mu.RLock()
	g := a.guard
	a.mu.RUnlock()
	if g == nil {
		return ""
	}
	return g(r.Context(), rc)
}

func New(log *slog.Logger) *Authenticator {
	return &Authenticator{log: log}
}

// SetRealms publishes discovered realms (called by background OIDC discovery
// so startup is never blocked on an unreachable issuer).
func (a *Authenticator) SetRealms(realms []Realm) {
	a.mu.Lock()
	a.realms = realms
	a.mu.Unlock()
}

// publicExact / publicPrefix are the permitAll set.
// /api/v1/auth/** is treated as public here because login is a separate concern
// (handled by Keycloak), not bearer-protected.
var publicExact = map[string]bool{
	"/error":                                 true,
	"/api/v1/platform-configuration/default": true,
	// MCP + its OAuth discovery doc: Enforce passes through (still pre-validating
	// any valid JWT into the RequestContext); the MCP handler does its own
	// dual-scheme gate (JWT realm or `Bearer pk.sk` api key) and emits the
	// RFC 9728 401 challenge itself.
	"/.well-known/oauth-protected-resource":          true,
	"/api/v1/admin/billing/configuration/countries":  true,
	"/api/v1/admin/billing/configuration/currencies": true,
}

var publicPrefix = []string{
	"/openapi.json",
	"/mcp", // MCP endpoint — auth enforced by the MCP handler (see publicExact note)
	"/api/v1/auth/",
	"/api/v1/download/",
	"/api/v1/console/", // VNC console WebSocket proxy — the console token is the auth
	"/api/v1/callbacks/",
	"/api/v1/admin/onboarding/",
}

// adminJobPrefix drives operator billing/collect/sync jobs — it is NOT public (anon must not be
// able to fire it) and must be an ADMIN credential (SigV4 or an admin/admin-api realm bearer),
// not merely any authenticated principal.
const adminJobPrefix = "/api/v1/admin/job/"

// IsPublic reports whether a path bypasses bearer enforcement.
func IsPublic(path string) bool {
	if publicExact[path] {
		return true
	}
	for _, p := range publicPrefix {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Enforce is the gate. Public paths pass with an empty RequestContext;
// protected paths require a valid credential or get a 401.
func (a *Authenticator) Enforce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsPublic(r.URL.Path) {
			// Public path: authentication is not REQUIRED, but a valid token is still processed
			// (the auth context is populated on permitAll paths too) — some public DTOs vary
			// by auth, e.g. platform-configuration returns `regions` only to authenticated callers.
			rc := &httpx.RequestContext{}
			// A refused identity is anonymous here; it never reaches a handler as itself.
			if got, ok := a.authenticate(r); ok && a.refusal(r, got) == "" {
				rc = got
			}
			next.ServeHTTP(w, r.WithContext(httpx.WithRC(r.Context(), rc)))
			return
		}
		rc, ok := a.authenticate(r)
		if !ok {
			unauthorized(w)
			return
		}
		if code := a.refusal(r, rc); code != "" {
			httpx.Err(w, http.StatusForbidden, http.StatusForbidden, code)
			return
		}
		// Operator job triggers require an ADMIN credential, not just any authenticated user.
		if strings.HasPrefix(r.URL.Path, adminJobPrefix) && !a.isAdmin(rc) {
			forbidden(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithRC(r.Context(), rc)))
	})
}

// isAdmin reports whether the credential is an admin one: a SigV4 hmac key (Admin API) or a bearer
// from the admin / admin-api realm (issuer match). A plain client-realm user is NOT admin.
func (a *Authenticator) isAdmin(rc *httpx.RequestContext) bool {
	if rc == nil {
		return false
	}
	if rc.SigV4KeyID != "" {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, realm := range a.realms {
		if realm.IssuerURI == "" || rc.Issuer != realm.IssuerURI {
			continue
		}
		if realm.Name == "admin" || realm.Name == "admin-api" {
			return true
		}
	}
	return false
}

// authenticate resolves a request to a RequestContext via the supported
// schemes: the OIDC JWT path and the SigV4 (Admin API) path; anything else
// fails closed.
func (a *Authenticator) authenticate(r *http.Request) (*httpx.RequestContext, bool) {
	authz := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(authz, "Bearer "):
		raw := strings.TrimPrefix(authz, "Bearer ")
		// A `Bearer pk.sk` client PAT (see clientkey.go) — a real JWT never matches this shape.
		if m := clientKeyRe.FindStringSubmatch(raw); m != nil {
			return a.verifyClientKey(r, m[1], m[2])
		}
		return a.verifyJWT(r, raw)
	case strings.HasPrefix(authz, "AWS4-HMAC-SHA256"):
		return a.verifySigV4(r, authz)
	default:
		return nil, false
	}
}

func (a *Authenticator) verifyJWT(r *http.Request, raw string) (*httpx.RequestContext, bool) {
	a.mu.RLock()
	realms := a.realms
	a.mu.RUnlock()
	// The /mcp resource is accessed by OAuth clients that register THEMSELVES (dynamic
	// client registration, or a dedicated MCP client), so their azp never matches the SPA
	// clientId. Skip the azp binding there — the token is still fully validated (signature,
	// issuer, expiry) and the MCP handler's issuer→toolset mapping is the real gate. Every
	// other path keeps the SPA/API azp binding.
	skipAzp := strings.HasPrefix(r.URL.Path, "/mcp")
	for _, realm := range realms {
		if realm.Verifier == nil {
			continue
		}
		tok, err := realm.Verifier.Verify(r.Context(), raw)
		if err != nil {
			continue
		}
		var claims struct {
			Sub           string `json:"sub"`
			Email         string `json:"email"`
			EmailVerified any    `json:"email_verified"`
			GivenName     string `json:"given_name"`
			FamilyName    string `json:"family_name"`
			Azp           string `json:"azp"`
			Scope         string `json:"scope"`
			IdP           string `json:"idp"`
			IdPGroups     any    `json:"idp_groups"`
		}
		if err := tok.Claims(&claims); err != nil {
			continue
		}
		// Bind the authorized party: a token minted for a DIFFERENT client in the same issuer
		// must not be accepted as this realm's principal. SkipClientIDCheck (aud=account public
		// clients) stays on the verifier; we bind azp here instead. Tokens that omit azp are
		// allowed (Keycloak stamps azp on client tokens, so a cross-client token still carries a
		// concrete, non-matching azp).
		if !skipAzp && !azpAllowed(realm.ClientID, claims.Azp) {
			continue
		}
		// Authenticate only — do NOT create the domain User here. The User is
		// created solely via POST /api/v1/user?id_token=... (PrincipalProvider);
		// the access token in the header never auto-initializes. Handlers that
		// need the User look it up and 400 "User is not initialized" if absent.
		return &httpx.RequestContext{
			Sub:           claims.Sub,
			Email:         claims.Email,
			EmailVerified: claimTrue(claims.EmailVerified),
			GivenName:     claims.GivenName,
			FamilyName:    claims.FamilyName,
			Issuer:        realm.IssuerURI,
			Azp:           claims.Azp,
			IdP:           claims.IdP,
			IdPGroups:     stringList(claims.IdPGroups),
			SSOScope:      slices.Contains(strings.Fields(claims.Scope), ssoScope),
		}, true
	}
	return nil, false
}

// ssoScope is the client scope that carries the idp and idp_groups claims.
const ssoScope = "proxcloud-sso"

// stringList reads a claim Keycloak renders as a string or a list depending on how many values it has.
func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// claimTrue reads a boolean claim. Keycloak sends a JSON bool; some IdPs send the
// string "true". Anything else, absent included, is false (fail closed).
func claimTrue(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	}
	return false
}

// azpAllowed reports whether a token's azp claim is acceptable for a realm: no configured client
// id (nothing to bind), or an absent azp, or an exact match. A present-but-mismatched azp (a token
// minted for another client in the same issuer) is rejected.
func azpAllowed(realmClientID, azp string) bool {
	return realmClientID == "" || azp == "" || azp == realmClientID
}

// forbidden writes a 403 with the nosniff guard (an authenticated but non-admin credential).
func forbidden(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
}

// unauthorized writes the bearer-token challenge response:
// 401, empty body, WWW-Authenticate: Bearer, X-Content-Type-Options: nosniff.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusUnauthorized)
}
