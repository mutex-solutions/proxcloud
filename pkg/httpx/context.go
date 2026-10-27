package httpx

import (
	"context"
	"net/http"
)

// RequestContext is the per-request tenant context (user + org + project +
// billing profile + services). Propagated via context.Context — NEVER a
// package global or goroutine-local — so it survives across goroutines without
// cross-tenant leakage. Populated by auth middleware.
type RequestContext struct {
	Sub        string
	Email      string
	GivenName  string
	FamilyName string
	Issuer     string
	Azp        string // authorized party (token azp claim) — gates the admin realm/client
	SigV4KeyID string // set when the request authenticated via AWS SigV4 (hmac_keys) — Admin API
	// Roles/permissions populate in later slices (Org/Project).
}

type ctxKey int

const rcKey ctxKey = iota

// WithRC returns a context carrying the request context.
func WithRC(ctx context.Context, rc *RequestContext) context.Context {
	return context.WithValue(ctx, rcKey, rc)
}

// RC extracts the request context (nil if unset).
func RC(ctx context.Context) *RequestContext {
	rc, _ := ctx.Value(rcKey).(*RequestContext)
	return rc
}

// FromRequest is a convenience for handlers.
func FromRequest(r *http.Request) *RequestContext { return RC(r.Context()) }
