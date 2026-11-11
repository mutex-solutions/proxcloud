//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/account"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/auth"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestStoredPATsAreHashed(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	pat := db.C("client_api_keys")
	mustInsert(t, db, "client_api_keys", pgdoc.M{"_id": "pk-old", "secretKey": "sk-old", "sub": "u1"})

	n, err := account.HashStoredPATs(ctx, pat)
	if err != nil || n != 1 {
		t.Fatalf("converted %d, %v", n, err)
	}
	var doc pgdoc.M
	if _, err := pat.Get(ctx, "pk-old", &doc); err != nil {
		t.Fatal(err)
	}
	if doc["secretKey"] != nil || doc["secretHash"] != auth.HashClientKeySecret("sk-old") {
		t.Fatalf("row = %v", doc)
	}
	if n, _ := account.HashStoredPATs(ctx, pat); n != 0 {
		t.Fatalf("second run converted %d", n)
	}
}

func serveAPIKeys(t *testing.T, db *pgdoc.DB, rc *httpx.RequestContext) *httptest.ResponseRecorder {
	t.Helper()
	mustInsert(t, db, "users", pgdoc.M{"_id": "user-1", "sub": rc.Sub, "email": "a@b.c"})
	r := chi.NewRouter()
	account.NewHandler(user.NewRepo(db), audit.NewService(audit.NewRepo(db), slog.Default()), db.C("client_api_keys")).Routes(r)
	req := httptest.NewRequest(http.MethodPost, "/user/api-keys", strings.NewReader(`{"expiresInDays":3650}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req.WithContext(httpx.WithRC(req.Context(), rc)))
	return w
}

// A short-lived token must not be able to renew itself.
func TestTokenCannotMintAToken(t *testing.T) {
	db := freshPG(t)
	w := serveAPIKeys(t, db, &httpx.RequestContext{Sub: "u1", Issuer: "i", PAT: true})
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	if n, _ := db.C("client_api_keys").Count(context.Background(), pgdoc.M{}); n != 0 {
		t.Fatalf("%d tokens minted", n)
	}
}

func TestMintedTokenStoredAsHash(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	w := serveAPIKeys(t, db, &httpx.RequestContext{Sub: "u1", Issuer: "i"})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Data struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	_, sk, ok := strings.Cut(out.Data.Token, ".")
	if !ok {
		t.Fatalf("token = %q", out.Data.Token)
	}
	var doc pgdoc.M
	if _, err := db.C("client_api_keys").Get(ctx, out.Data.ID, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["secretKey"] != nil || doc["secretHash"] != auth.HashClientKeySecret(sk) {
		t.Fatalf("row = %v", doc)
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), sk) {
		t.Fatal("the secret is in the row")
	}
}
