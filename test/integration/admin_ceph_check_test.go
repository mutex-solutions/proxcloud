//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// A stored object-store row that cannot reach its store blocks provisioning of every new project, so the console's writes
// prove it first, whatever its status. Writes that leave the connection alone must still work with RGW down.
func TestConsoleChecksObjectStoreRow(t *testing.T) {
	var keysSeen []string
	rgwUp := true
	rgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rgwUp {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin/user") {
			keysSeen = append(keysSeen, r.URL.Query().Get("access-key"))
			_, _ = w.Write([]byte(`{"caps":[{"type":"users","perm":"*"},{"type":"buckets","perm":"*"},{"type":"usage","perm":"*"}]}`))
			return
		}
		_, _ = w.Write([]byte(`<ListAllMyBucketsResult><Buckets/></ListAllMyBucketsResult>`))
	}))
	defer rgw.Close()

	db := freshPG(t)
	const iss, cid = "test-iss", "test-cid"
	es := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, es, "", iss, cid)
	r := chi.NewRouter()
	h.Routes(r)
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	row := func(endpoint, status string) map[string]any {
		return map[string]any{
			"name": "rgw", "type": "CLOUD", "status": status,
			"config": map[string]any{"provider": "ceph-s3", "s3Endpoint": endpoint, "adminApiUrl": endpoint + "/admin", "region": "r1"},
			"secret": map[string]any{"adminAccessKey": "AK", "adminSecretKey": "SK"},
		}
	}
	stored := func() int64 {
		n, err := db.C("externalService").Count(t.Context(), pgdoc.M{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	for _, status := range []string{"PUBLIC", "PRIVATE", "DISABLED"} {
		if w := do(http.MethodPost, "/admin/service", row("http://127.0.0.1:1", status)); w.Code != http.StatusBadRequest || stored() != 0 {
			t.Fatalf("bad %s endpoint: status %d, stored %d: %s", status, w.Code, stored(), w.Body)
		}
	}
	rustfs := row("http://127.0.0.1:1", "PRIVATE")
	rustfs["config"].(map[string]any)["provider"] = "rustfs"
	if w := do(http.MethodPost, "/admin/service", rustfs); w.Code != http.StatusBadRequest || stored() != 0 {
		t.Fatalf("bad rustfs endpoint: status %d, stored %d: %s", w.Code, stored(), w.Body)
	}
	w := do(http.MethodPost, "/admin/service", row(rgw.URL, "PUBLIC"))
	if w.Code != http.StatusOK || stored() != 1 {
		t.Fatalf("good row: status %d: %s", w.Code, w.Body)
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.ID == "" {
		t.Fatalf("create answer: %v: %s", err, w.Body)
	}
	path := "/admin/service/" + created.Data.ID

	// A stored secret is ciphertext: the check must see the real key, and with no new key in the body too.
	keysSeen = nil
	if w := do(http.MethodPut, path, map[string]any{"config": map[string]any{"region": "r2"}}); w.Code != http.StatusOK {
		t.Fatalf("good update: status %d: %s", w.Code, w.Body)
	}
	if len(keysSeen) != 1 || keysSeen[0] != "AK" {
		t.Fatalf("update must check with the stored key decrypted, saw %q", keysSeen)
	}
	if w := do(http.MethodPut, path, map[string]any{"config": map[string]any{"s3Endpoint": "http://127.0.0.1:1"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("bad update: status %d: %s", w.Code, w.Body)
	}

	// A stored row is tested with its stored key, and the answer follows the store.
	keysSeen = nil
	if w := do(http.MethodPost, path+"/object-store/test", nil); w.Code != http.StatusOK || len(keysSeen) != 1 || keysSeen[0] != "AK" {
		t.Fatalf("test of a stored row: status %d, keys %q: %s", w.Code, keysSeen, w.Body)
	}
	if w := do(http.MethodPost, "/admin/service/no-such-row/object-store/test", nil); w.Code == http.StatusOK {
		t.Fatalf("test of a missing row: status %d", w.Code)
	}
	var doc pgdoc.M
	if _, err := db.C("externalService").Get(t.Context(), created.Data.ID, &doc); err != nil {
		t.Fatal(err)
	}
	if secret, _ := doc["secret"].(map[string]any); secret["adminAccessKey"] == "AK" {
		t.Fatal("a check must not leave the secret decrypted in the store")
	}

	rgwUp = false
	if w := do(http.MethodPost, path+"/object-store/test", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("test of a stored row with RGW down: status %d: %s", w.Code, w.Body)
	}
	if w := do(http.MethodPut, path, map[string]any{"config": map[string]any{"services": map[string]any{}}}); w.Code != http.StatusOK {
		t.Fatalf("RGW down, connection untouched: status %d: %s", w.Code, w.Body)
	}
	if w := do(http.MethodPut, path+"/configuration", map[string]any{"name": "rgw", "status": "DISABLED"}); w.Code != http.StatusOK {
		t.Fatalf("RGW down, disabling: status %d: %s", w.Code, w.Body)
	}
	if w := do(http.MethodPut, path+"/configuration", map[string]any{"name": "rgw", "status": "PUBLIC"}); w.Code != http.StatusBadRequest {
		t.Fatalf("RGW down, publishing: status %d: %s", w.Code, w.Body)
	}
}
