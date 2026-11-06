package auth

// clientkey.go authenticates a client Personal Access Token (PAT) presented as
// `Authorization: Bearer <pk>.<sk>` — a self-service credential a client-web user mints to reach
// the client API and the client MCP toolset (e.g. from a Terraform module) without an interactive
// SSO session.
//
// The token behaves EXACTLY like that user's OIDC session downstream: the principal is the token's
// owning user (Sub) scoped to the main (client) realm issuer, so every handler's own-record / org /
// project policy applies unchanged and the token is never treated as admin (isAdmin checks the
// admin/admin-api realms only). PATs live in a SEPARATE collection (client_api_keys) from the admin
// hmac_keys — physical isolation, so a client PAT can never resolve on the Admin-API SigV4 path and
// an admin key can never resolve here.

import (
	"crypto/subtle"
	"net/http"
	"regexp"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// ClientKeyLookup resolves a client PAT access-key id (pk…) to its secret half and the owning user's
// subject. ok=false when the key is unknown or revoked. sub scopes the resulting principal.
type ClientKeyLookup func(r *http.Request, accessKeyID string) (secret, sub string, ok bool)

// SetClientKeyLookup wires the client_api_keys resolver (nil → PAT bearers are rejected).
func (a *Authenticator) SetClientKeyLookup(l ClientKeyLookup) {
	a.mu.Lock()
	a.clientKeyLookup = l
	a.mu.Unlock()
}

// clientKeyRe matches a client PAT bearer: "pk<32hex>.sk<40hex>" (same shape as the admin hmac
// pair). A real OIDC JWT never matches this, so authenticate can branch on it before verifyJWT.
var clientKeyRe = regexp.MustCompile(`^(pk[0-9a-f]{32})\.(sk[0-9a-f]{40})$`)

// verifyClientKey authenticates a `Bearer pk.sk` client PAT. On success the RequestContext carries
// the owning user's Sub and the main (client) realm issuer, so it is indistinguishable from that
// user's JWT to the rest of the stack (client tools, own projects/orgs — never admin).
func (a *Authenticator) verifyClientKey(r *http.Request, pk, sk string) (*httpx.RequestContext, bool) {
	a.mu.RLock()
	lookup := a.clientKeyLookup
	issuer := ""
	for _, realm := range a.realms {
		if realm.Name == "main" {
			issuer = realm.IssuerURI
			break
		}
	}
	a.mu.RUnlock()
	// Fail closed: no resolver, or the main (client) realm hasn't been discovered yet — a PAT must
	// not authenticate with an empty issuer (it would escape the intended main-realm scoping).
	if lookup == nil || issuer == "" {
		return nil, false
	}
	secret, sub, ok := lookup(r, pk)
	// Constant-time compare of the secret half; sub must resolve to a user (empty = reject).
	if !ok || sub == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(sk)) != 1 {
		return nil, false
	}
	return &httpx.RequestContext{Sub: sub, Issuer: issuer, PAT: true}, true
}
