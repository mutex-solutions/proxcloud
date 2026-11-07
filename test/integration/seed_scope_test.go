//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/adminapi"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// A vmid reused by another project: tenant B's server must not read tenant A's seed, user-data and all.
func TestSeedOfAReusedVMIDStaysWithItsProject(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := project.NewSeedRepo(db, textcrypt.New("k"))
	mustInsert(t, db, "pveServerSeed", pgdoc.M{"_id": "seed-a", "serviceId": "svc", "projectId": "tenant-a", "serverId": "106", "userData": "secret of a"})

	got, err := repo.ForServer(ctx, "svc", "tenant-b", "106")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("tenant B got tenant A's seed: %+v", got)
	}
	got, err = repo.ForServer(ctx, "svc", "tenant-a", "106")
	if err != nil || got == nil || got.ID != "seed-a" {
		t.Fatalf("tenant A: %+v, %v", got, err)
	}
}

// A CD that cannot be emptied keeps a row so the sync can retry, but no user-data, and no claim on the vmid.
func TestFailedSeedRemovalIsRetried(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	seeds := project.NewSeedRepo(db, textcrypt.New("k"))
	mustInsert(t, db, "pveServerSeed", pgdoc.M{"_id": "seed-a", "serviceId": "svc", "projectId": "p", "serverId": "106", "node": "a", "mode": "iso",
		"iso": "iso:iso/pcseed-106-x.iso", "passwordHash": "h", "userData": textcrypt.New("k").Encrypt("secret")})

	down := true
	reply := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux := http.NewServeMux()
	mux.HandleFunc("/api2/json/storage", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{{"storage": "iso", "type": "dir", "content": "iso"}})
	})
	mux.HandleFunc("/api2/json/cluster/resources", func(w http.ResponseWriter, _ *http.Request) { reply(w, []any{}) })
	mux.HandleFunc("/api2/json/nodes/a/storage/iso/content/", func(w http.ResponseWriter, _ *http.Request) {
		if down {
			http.Error(w, "no route to host", 595)
			return
		}
		reply(w, nil)
	})
	mux.HandleFunc("/api2/json/nodes/a/storage/iso/upload", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no route to host", 595)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "svc", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	h := project.NewHandler(project.NewService(project.NewRepo(db), nil, nil, nil, nil), nil, nil, nil, nil, nil, cloud.NewRepo(db), nil, nil, nil, "r")
	h.SetSeeds(seeds)
	es := &externalservice.ExternalService{ID: "svc"}

	if err := h.DropStaleSeeds(ctx, ps, es); err != nil {
		t.Fatal(err)
	}
	var kept []project.ServerSeed
	if err := db.C("pveServerSeed").Find(ctx, pgdoc.M{"serviceId": "svc"}, &kept); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].ISO != "iso:iso/pcseed-106-x.iso" || kept[0].UserData != "" || kept[0].PasswordHash != "" {
		t.Fatalf("kept %+v", kept)
	}
	// The next server with this vmid must not read it.
	if got, err := seeds.ForServer(ctx, "svc", "p", "106"); err != nil || got != nil {
		t.Fatalf("reused vmid got %+v, %v", got, err)
	}

	down = false
	if err := h.DropStaleSeeds(ctx, ps, es); err != nil {
		t.Fatal(err)
	}
	kept = nil
	if err := db.C("pveServerSeed").Find(ctx, pgdoc.M{"serviceId": "svc"}, &kept); err != nil || len(kept) != 0 {
		t.Fatalf("after the retry: %+v, %v", kept, err)
	}
}

// A retype keeps the row and so the id the bill is keyed on, and a row the sync made too early goes.
func TestChangeExternalIDKeepsTheRow(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	old, err := repo.Insert(ctx, &cloud.CloudResource{ServiceID: "svc", ProjectID: "p", Type: cloud.TypeVolume, ExternalID: "fast:vm-1-disk-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Insert(ctx, &cloud.CloudResource{ServiceID: "svc", ProjectID: "p", Type: cloud.TypeVolume, ExternalID: "slow:vm-1-disk-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ChangeExternalID(ctx, old, "slow:vm-1-disk-1"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.FindByServiceIDAndExternalID(ctx, "svc", "slow:vm-1-disk-1")
	if err != nil || got == nil || got.ID != old.ID {
		t.Fatalf("row = %+v (%v), want id %s", got, err, old.ID)
	}
	if rows, _ := repo.FindByServiceAndType(ctx, "svc", cloud.TypeVolume); len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	if gone, _ := repo.FindByServiceIDAndExternalID(ctx, "svc", "fast:vm-1-disk-1"); gone != nil {
		t.Fatal("the old volid still resolves")
	}
}

type fakeAccounts struct {
	deleted []string
	err     error
}

func (f *fakeAccounts) CreateUser(context.Context, string, string, string) (string, string, error) {
	return "kc-sub-1", "temp-pass", f.err
}
func (f *fakeAccounts) DeleteUser(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// A user created with no sub has a login, or is refused: never a row no login can match.
func TestAdminAPIUserCreateMakesALogin(t *testing.T) {
	db := freshPG(t)
	h := adminapi.NewHandler(db, nil, user.NewRepo(db), nil, audit.NewService(audit.NewRepo(db), slog.Default()), "", "")
	r := chi.NewRouter()
	h.Routes(r)
	signed := func(req *http.Request) *http.Request {
		return req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{SigV4KeyID: "pk-test"}))
	}
	post := func(body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/users", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, signed(req))
		var out struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.Data
	}
	if code, _ := post(`{"email":"a@example.com"}`); code != http.StatusBadRequest {
		t.Fatalf("no service account, no sub: %d", code)
	}
	if code, data := post(`{"email":"b@example.com","sub":"ext-1"}`); code != http.StatusOK || data["sub"] != "ext-1" || data["temporary_password"] != nil {
		t.Fatalf("with sub: %d %v", code, data)
	}
	acc := &fakeAccounts{}
	h.SetKeycloak(acc)
	code, data := post(`{"email":"C@example.com "}`)
	if code != http.StatusOK || data["sub"] != "kc-sub-1" || data["temporary_password"] != "temp-pass" || data["email"] != "c@example.com" {
		t.Fatalf("with service account: %d %v", code, data)
	}
	acc.err = errors.New("keycloak down")
	if code, _ := post(`{"email":"d@example.com"}`); code != http.StatusBadGateway {
		t.Fatalf("keycloak down: %d", code)
	}
	if n, _ := db.C("users").Count(context.Background(), pgdoc.M{"email": "d@example.com"}); n != 0 {
		t.Fatal("a row without a login")
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, signed(httptest.NewRequest(http.MethodDelete, "/users/"+data["id"].(string), nil)))
	if w.Code != http.StatusOK || len(acc.deleted) != 1 || acc.deleted[0] != "kc-sub-1" {
		t.Fatalf("delete: %d %v", w.Code, acc.deleted)
	}
}
