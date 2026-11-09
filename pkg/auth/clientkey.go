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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"regexp"
	"time"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// PAT scopes: full acts as the user; read can only read.
const (
	PATScopeFull = "full"
	PATScopeRead = "read"
)

// ClientKey is a resolved client PAT.
type ClientKey struct {
	SecretHash string // HashClientKeySecret of the secret half: the secret itself is never stored
	Sub        string // scopes the resulting principal
	Scope      string // "" = PATScopeFull
	// ExpiresAt zero = never.
	ExpiresAt time.Time
}

// ClientKeyLookup resolves a client PAT access-key id (pk…). ok=false when the key is unknown or
// revoked.
type ClientKeyLookup func(r *http.Request, accessKeyID string) (key ClientKey, ok bool)

// SetClientKeyLookup wires the client_api_keys resolver (nil → PAT bearers are rejected).
func (a *Authenticator) SetClientKeyLookup(l ClientKeyLookup) {
	a.mu.Lock()
	a.clientKeyLookup = l
	a.mu.Unlock()
}

// HashClientKeySecret is what the store keeps of a PAT's secret half. A bearer token is only ever
// compared, never used to sign, so nothing needs it back; SHA-256 is enough for 160 random bits.
func HashClientKeySecret(sk string) string {
	sum := sha256.Sum256([]byte(sk))
	return hex.EncodeToString(sum[:])
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
	key, ok := lookup(r, pk)
	// Constant-time compare of the secret half; sub must resolve to a user (empty = reject).
	if !ok || key.Sub == "" || subtle.ConstantTimeCompare([]byte(key.SecretHash), []byte(HashClientKeySecret(sk))) != 1 {
		return nil, false
	}
	if !key.ExpiresAt.IsZero() && !time.Now().Before(key.ExpiresAt) {
		return nil, false
	}
	if key.Scope == PATScopeRead && !patReadAllowed(r) {
		return nil, false
	}
	return &httpx.RequestContext{Sub: key.Sub, Issuer: issuer, PAT: true}, true
}

// patReadPosts are the POST routes that only read: the resource list takes its filters in the body.
var patReadPosts = regexp.MustCompile(`^/api/v1/project/[^/]+/resource$`)

// patSecretReads are the GET routes that hand out a live credential. Read access to a project's
// resources must not become write access to its buckets.
var patSecretReads = regexp.MustCompile(`/(s3-credentials|s3-keys)(/|$)`)

// patReadAllowed reports whether a read-only PAT may make this request: safe methods except the
// credential reads, plus the POST list route. Everything else, /mcp included (its tools can write), is
// refused. Matched on the exact route: a suffix would also let in a write that ends the same way.
func patReadAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return !patSecretReads.MatchString(r.URL.Path)
	case http.MethodPost:
		return patReadPosts.MatchString(r.URL.Path)
	}
	return false
}
