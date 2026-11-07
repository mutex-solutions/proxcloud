//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

func storedSecret(t *testing.T, db *pgdoc.DB, id string) string {
	t.Helper()
	var doc struct {
		SecretKey string `json:"secretKey"`
	}
	if found, err := db.C("hmac_keys").FindOne(context.Background(), pgdoc.M{"_id": id}, &doc); err != nil || !found {
		t.Fatalf("row %s: found=%v err=%v", id, found, err)
	}
	return doc.SecretKey
}

// TestSealStoredHmacKeys converts plain rows once, and the secret still opens afterwards.
func TestSealStoredHmacKeys(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	plain := "sk" + strings.Repeat("ab", 20)
	mustInsert(t, db, "hmac_keys", pgdoc.M{"_id": "pkold", "secretKey": plain, "purpose": "admin-api"})
	fresh := admin.NewAdminAPIKey("new")
	freshID, _ := fresh["_id"].(string)
	freshPlain, _ := fresh["secretKey"].(string)
	mustInsert(t, db, "hmac_keys", admin.SealHmacKey(enc, fresh))

	if s := storedSecret(t, db, freshID); s == freshPlain || strings.HasPrefix(s, "sk") {
		t.Fatalf("a new key is stored as %q", s)
	}
	if fresh["secretKey"] != freshPlain {
		t.Fatal("sealing changed the doc the one-time response reads")
	}

	n, err := admin.SealStoredHmacKeys(ctx, db.C("hmac_keys"), enc)
	if err != nil || n != 1 {
		t.Fatalf("converted %d, %v; want the one plain row", n, err)
	}
	sealed := storedSecret(t, db, "pkold")
	if sealed == plain {
		t.Fatal("the plain row is still plain")
	}
	if got, ok := admin.OpenHmacSecret(enc, sealed); !ok || got != plain {
		t.Fatalf("open = %q, %v", got, ok)
	}
	if got, ok := admin.OpenHmacSecret(enc, storedSecret(t, db, freshID)); !ok || got != freshPlain {
		t.Fatalf("open new = %q, %v", got, ok)
	}

	if n, err := admin.SealStoredHmacKeys(ctx, db.C("hmac_keys"), enc); err != nil || n != 0 {
		t.Fatalf("second run converted %d, %v", n, err)
	}
	if storedSecret(t, db, "pkold") != sealed {
		t.Fatal("the second run sealed a sealed row again")
	}

	// No key: nothing is claimed to be protected, and nothing breaks.
	if n, err := admin.SealStoredHmacKeys(ctx, db.C("hmac_keys"), textcrypt.New("")); err != nil || n != 0 {
		t.Fatalf("with no key: %d, %v", n, err)
	}
}

// TestOpenHmacSecretFailsClosed: under another key the stored text must not become the secret.
func TestOpenHmacSecretFailsClosed(t *testing.T) {
	plain := "sk" + strings.Repeat("cd", 20)
	sealed := textcrypt.New("old-key").Encrypt(plain)
	if got, ok := admin.OpenHmacSecret(textcrypt.New("new-key"), sealed); ok {
		t.Fatalf("opened under the wrong key: %q", got)
	}
	if _, ok := admin.OpenHmacSecret(textcrypt.New(""), sealed); ok {
		t.Fatal("opened with no key")
	}
	if got, ok := admin.OpenHmacSecret(textcrypt.New("new-key"), plain); !ok || got != plain {
		t.Fatalf("a row from before sealing: %q, %v", got, ok)
	}
}

// TestAdminGeneratedHmacKeyIsSealed drives the operator's endpoint: the response shows the secret once, the row
// does not hold it, and the reads never return it.
func TestAdminGeneratedHmacKeyIsSealed(t *testing.T) {
	db := freshPG(t)
	const iss, cid = "https://kc/realms/master", "admin"
	handler := admin.NewHandler(admin.NewRepo(db), nil, user.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, "", iss, cid)
	enc := textcrypt.New("test-key")
	handler.SetHmacEncryptor(enc)
	h := chi.NewRouter()
	handler.Routes(h)
	rc := &httpx.RequestContext{Sub: "op-1", Issuer: iss, Azp: cid}

	w := doAdmin(h, rc, "POST", "/admin/hmac-keys", `{"description":"x"}`)
	if w.Code != 200 {
		t.Fatalf("generate: %d %s", w.Code, w.Body)
	}
	data := dataOf(t, w)
	id, _ := data["id"].(string)
	secret, _ := data["secretKey"].(string)
	if !strings.HasPrefix(secret, "sk") {
		t.Fatalf("the response's secret = %q", secret)
	}
	stored := storedSecret(t, db, id)
	if stored == secret || strings.Contains(stored, secret) {
		t.Fatal("the row holds the secret in plain text")
	}
	if got, ok := admin.OpenHmacSecret(enc, stored); !ok || got != secret {
		t.Fatalf("open = %q, %v", got, ok)
	}
	for _, path := range []string{"/admin/hmac-keys", "/admin/hmac-keys/" + id} {
		w := doAdmin(h, rc, "GET", path, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "secretKey") || strings.Contains(w.Body.String(), stored) {
			t.Errorf("%s returned the secret field: %s", path, w.Body)
		}
	}
}
