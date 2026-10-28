package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// download.go = the short-lived download token. A mutating cloud action (e.g. object-store
// DOWNLOAD) mints a token carrying the service/project + the resource externalId + per-type
// metadata; the whitelisted GET /api/v1/download/{token} resolves it + streams the bytes. The
// token IS the auth, so it MUST be unguessable: it is a 256-bit crypto/rand value returned to the
// caller, while only its SHA-256 hash is persisted (as `_id`) — a leaked DB never yields a usable
// token, and the public token is not derived from a sequential/guessable id.

// randomToken mints a 256-bit crypto-random download token (hex-encoded).
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hashToken is the at-rest form of a download token (only the hash is stored / matched).
func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// CloudDownload.Type values. The OBJECT path streams bucket-object bytes; the CONSOLE type carries
// a server's PVE vncwebsocket URL so the whitelisted console route can reverse-proxy the WebSocket
// (see cloudConsoleProxy) — the object-download handler rejects any non-download type.
const (
	DownloadTypeObject  = "OBJECT"
	DownloadTypeConsole = "CONSOLE"
)

// ConsoleTokenTTL bounds a console token's life. The PVE vncticket it fronts is short-lived,
// so a longer ProxCloud token would outlive the backend session.
const ConsoleTokenTTL = 10 * time.Minute

// CloudDownload is the cloudDownload collection document.
type CloudDownload struct {
	ID         string            `json:"id,omitempty"`
	Type       string            `json:"type,omitempty"`
	ServiceID  string            `json:"serviceId,omitempty"`
	ProjectID  string            `json:"projectId,omitempty"`
	ExternalID string            `json:"externalId,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  *time.Time        `json:"createdAt,omitempty"`
	ExpiresAt  *time.Time        `json:"expiresAt,omitempty"`
}

// DownloadRepo persists download tokens.
type DownloadRepo struct{ col *pgdoc.Store }

func NewDownloadRepo(db *pgdoc.DB) *DownloadRepo {
	return &DownloadRepo{col: db.C("cloudDownload")}
}

// Create mints a download token: a 1-hour TTL doc. The persisted `_id` is the HASH of a
// crypto-random token; the returned d.ID carries the RAW token (the public download token) — it is
// never stored, so a DB read cannot reconstruct a usable token.
func (r *DownloadRepo) Create(ctx context.Context, d *CloudDownload) (*CloudDownload, error) {
	return r.CreateWithTTL(ctx, d, time.Hour)
}

// CreateWithTTL is Create with an explicit lifetime — console tokens front a short-lived PVE
// console session and use ConsoleTokenTTL rather than the download default.
func (r *DownloadRepo) CreateWithTTL(ctx context.Context, d *CloudDownload, ttl time.Duration) (*CloudDownload, error) {
	now := time.Now().UTC()
	exp := now.Add(ttl)
	d.CreatedAt = &now
	d.ExpiresAt = &exp
	raw, err := randomToken()
	if err != nil {
		return nil, err
	}
	d.ID = hashToken(raw) // stored _id = hash(token)
	if _, err := r.col.InsertOne(ctx, d); err != nil {
		return nil, err
	}
	d.ID = raw // hand the caller the raw token (for the /api/v1/download/{token} URL)
	return d, nil
}

// ByID resolves a download token by hashing it and matching the stored hash (nil when absent /
// expired). The parameter is the raw public token from the URL.
func (r *DownloadRepo) ByID(ctx context.Context, id string) (*CloudDownload, error) {
	var d CloudDownload
	found, err := r.col.Get(ctx, hashToken(id), &d)
	if err != nil || !found {
		return nil, err
	}
	if d.ExpiresAt != nil && d.ExpiresAt.Before(time.Now().UTC()) {
		return nil, nil
	}
	return &d, nil
}
