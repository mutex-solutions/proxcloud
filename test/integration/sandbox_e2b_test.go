//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// newE2BFixture is newSbxFixture with the E2B API on: the provider runs envd under a domain, the keys store is wired,
// /e2b is mounted, and sandboxes become Ready at once. It returns a sandbox API key of tenant A (project pa) and one of
// tenant B (project pb), minted through the client API.
func newE2BFixture(t *testing.T, gates func(context.Context, *externalservice.ExternalService) []sandbox.Gate) (*sbxFixture, string, string) {
	t.Helper()
	f := newSbxFixture(t, gates)
	f.cluster.autoReady = true
	if _, err := f.db.C("externalService").SetByID(context.Background(), "sbx", pgdoc.M{
		"config": pgdoc.M{"provider": "sandbox", "region": "az1", "services": pgdoc.M{"sandbox": pgdoc.M{"az1": true}},
			"sandbox": pgdoc.M{"envd": true, "domain": "sbx.example.org",
				"templates": pgdoc.M{"base": pgdoc.M{"image": "base:1", "cpu": 1, "memoryGiB": 1}}}}}, nil); err != nil {
		t.Fatal(err)
	}
	f.h.SetSandboxKeys(f.db.C("sandbox_api_keys"))
	f.router.Route("/e2b", f.h.E2BRoutes)
	return f, f.mintKey(sbxA, "pa"), f.mintKey(sbxB, "pb")
}

func (f *sbxFixture) mintKey(sub, proj string) string {
	f.t.Helper()
	code, body := f.do(sub, http.MethodPost, "/project/"+proj+"/sandbox-api-keys", `{"name":"ci"}`)
	var env struct {
		Data struct {
			ID, Key string
		} `json:"data"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &env) != nil || !strings.HasPrefix(env.Data.Key, "e2b_") {
		f.t.Fatalf("mint key: %d %s", code, body)
	}
	return env.Data.Key
}

// e2b calls the E2B API with key (no user session: the key is the only credential).
func (f *sbxFixture) e2b(key, method, path, body string) (int, string) {
	req := httptest.NewRequest(method, "/e2b"+path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// e2bCreate starts a base sandbox and returns its id.
func (f *sbxFixture) e2bCreate(key string) string {
	f.t.Helper()
	code, body := f.e2b(key, http.MethodPost, "/sandboxes", `{"templateID":"base","timeout":300}`)
	var sb struct{ SandboxID, EnvdAccessToken, Domain string }
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sb) != nil || sb.SandboxID == "" {
		f.t.Fatalf("e2b create: %d %s", code, body)
	}
	if sb.EnvdAccessToken == "" || sb.Domain != "sbx.example.org" {
		f.t.Fatalf("create answer lacks envd's token or the domain: %s", body)
	}
	return sb.SandboxID
}

// The E2B routes are a second door to the same sandboxes: what adds usage (create, a timeout, an extension through
// refreshes or connect) passes the suspension gate, the provider's DISABLED state, the release gates and the quota,
// exactly like the native routes; reads and the kill stay open.
func TestE2BUsageGatesFailClosed(t *testing.T) {
	for _, c := range []struct {
		name  string
		gates func(context.Context, *externalservice.ExternalService) []sandbox.Gate
	}{
		{"no checker", nil},
		{"a gate fails", func(context.Context, *externalservice.ExternalService) []sandbox.Gate {
			return []sandbox.Gate{{Name: sandbox.GateFence, OK: false, Detail: "a worker is not fenced"}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, keyA, _ := newE2BFixture(t, c.gates)
			if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base"}`); code != http.StatusServiceUnavailable || f.cluster.count() != 0 {
				t.Fatalf("create: %d %s, %d on the cluster", code, body, f.cluster.count())
			}
		})
	}

	extend := []struct{ name, method, path, body string }{
		{"timeout", http.MethodPost, "/timeout", `{"timeout":3600}`},
		{"refreshes", http.MethodPost, "/refreshes", `{"duration":3600}`},
		{"connect", http.MethodPost, "/connect", `{"timeout":3600}`},
		{"v2 connect", http.MethodPost, "/v2/connect", `{"timeout":3600}`},
	}
	path := func(id, p string) string {
		if strings.HasPrefix(p, "/v2/") {
			return "/v2/sandboxes/" + id + strings.TrimPrefix(p, "/v2")
		}
		return "/sandboxes/" + id + p
	}

	t.Run("provider disabled", func(t *testing.T) {
		f, keyA, _ := newE2BFixture(t, gatesHold)
		id := f.e2bCreate(keyA)
		f.setProvider(externalservice.StatusDisabled)
		if code, body := f.e2b(keyA, http.MethodPost, "/v2/sandboxes", `{"templateID":"base"}`); code != http.StatusServiceUnavailable {
			t.Errorf("create on a DISABLED provider: %d %s", code, body)
		}
		for _, c := range extend {
			if code, body := f.e2b(keyA, c.method, path(id, c.path), c.body); code != http.StatusServiceUnavailable {
				t.Errorf("%s on a DISABLED provider: %d %s", c.name, code, body)
			}
		}
		if f.cluster.count() != 1 {
			t.Fatalf("a create got through: %d sandboxes", f.cluster.count())
		}
		for _, p := range []string{"/sandboxes/" + id, "/sandboxes/" + id + "/logs", "/v2/sandboxes"} {
			if code, body := f.e2b(keyA, http.MethodGet, p, ""); code != http.StatusOK {
				t.Errorf("GET %s: %d %s", p, code, body)
			}
		}
		// A connect that asks for no more time adds no usage.
		if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/connect", `{"timeout":10}`); code != http.StatusOK {
			t.Errorf("connect within the current end: %d %s", code, body)
		}
		if code, body := f.e2b(keyA, http.MethodDelete, "/sandboxes/"+id, ""); code != http.StatusNoContent || f.cluster.count() != 0 {
			t.Errorf("kill: %d %s", code, body)
		}
	})

	t.Run("profile suspended, project disabled", func(t *testing.T) {
		f, keyA, _ := newE2BFixture(t, gatesHold)
		id := f.e2bCreate(keyA)
		if _, err := f.db.C("billingProfile").SetByID(context.Background(), "bpa", pgdoc.M{"status": billing.StatusSuspended}, nil); err != nil {
			t.Fatal(err)
		}
		if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base"}`); code != http.StatusForbidden {
			t.Errorf("create on a suspended profile: %d %s", code, body)
		}
		if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes/"+id+"/timeout", `{"timeout":3600}`); code != http.StatusForbidden {
			t.Errorf("timeout on a suspended profile: %d %s", code, body)
		}
		if _, err := f.db.C("project").SetByID(context.Background(), "pa", pgdoc.M{"status": project.StatusDisabled}, nil); err != nil {
			t.Fatal(err)
		}
		if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base"}`); code != http.StatusConflict {
			t.Errorf("create on a DISABLED project: %d %s", code, body)
		}
		for _, c := range extend {
			if code, body := f.e2b(keyA, c.method, path(id, c.path), c.body); code != http.StatusConflict {
				t.Errorf("%s on a DISABLED project: %d %s", c.name, code, body)
			}
		}
		if f.cluster.count() != 1 {
			t.Fatalf("a create got through: %d sandboxes", f.cluster.count())
		}
		if code, body := f.e2b(keyA, http.MethodDelete, "/sandboxes/"+id, ""); code != http.StatusNoContent {
			t.Errorf("kill on a DISABLED project: %d %s", code, body)
		}
	})

	t.Run("quota", func(t *testing.T) {
		f, keyA, _ := newE2BFixture(t, gatesHold)
		if _, err := f.db.C("project").SetByID(context.Background(), "pa", pgdoc.M{"quota": pgdoc.M{"sandboxes": 1}}, nil); err != nil {
			t.Fatal(err)
		}
		f.e2bCreate(keyA)
		if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base"}`); code != http.StatusTooManyRequests || f.cluster.count() != 1 {
			t.Errorf("over quota: %d %s, %d on the cluster", code, body, f.cluster.count())
		}
		if code, body := f.e2b(keyA, http.MethodPost, "/sandboxes", `{"templateID":"base","cpuCount":99}`); code != http.StatusBadRequest {
			t.Errorf("over the per-sandbox limit: %d %s", code, body)
		}
	})
}

// A key reaches its own project only, is stored as a hash, stops at once when revoked, and stops when its creator
// loses the project. The create records the start on the row, so a sandbox only the E2B API ever touched is billed.
func TestE2BKeys(t *testing.T) {
	f, keyA, keyB := newE2BFixture(t, gatesHold)
	id := f.e2bCreate(keyA)

	rows, err := f.rows.FindByServiceProjectAndType(context.Background(), "sbx", "pa", "SANDBOX")
	if err != nil || len(rows) != 1 || rows[0].Data["runningSince"] == nil {
		t.Fatalf("row after an E2B create: %v %+v", err, rows)
	}

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/sandboxes/" + id},
		{http.MethodGet, "/sandboxes/" + id + "/logs"},
		{http.MethodPost, "/sandboxes/" + id + "/timeout"},
		{http.MethodPost, "/sandboxes/" + id + "/connect"},
		{http.MethodDelete, "/sandboxes/" + id},
	} {
		if code, body := f.e2b(keyB, c.method, c.path, `{"timeout":60}`); code != http.StatusNotFound || strings.Contains(body, "envdAccessToken") {
			t.Errorf("tenant B %s %s: %d %s", c.method, c.path, code, body)
		}
	}
	if code, body := f.e2b(keyB, http.MethodGet, "/sandboxes", ""); code != http.StatusOK || strings.Contains(body, id) {
		t.Errorf("tenant B's list: %d %s", code, body)
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes", ""); code != http.StatusOK || !strings.Contains(body, id) {
		t.Errorf("tenant A's list: %d %s", code, body)
	}

	// Only the hash is stored; the list shows a hint, never the key.
	var docs []pgdoc.M
	if err := f.db.C("sandbox_api_keys").Find(context.Background(), pgdoc.M{}, &docs); err != nil || len(docs) != 2 {
		t.Fatalf("stored keys: %v %d", err, len(docs))
	}
	raw, _ := json.Marshal(docs)
	if strings.Contains(string(raw), keyA) || strings.Contains(string(raw), keyB) {
		t.Fatal("a key is stored in clear")
	}
	code, body := f.do(sbxA, http.MethodGet, "/project/pa/sandbox-api-keys", "")
	if code != http.StatusOK || strings.Contains(body, keyA) || strings.Contains(body, keyB) {
		t.Fatalf("key list: %d %s", code, body)
	}
	if code, body := f.do(sbxB, http.MethodGet, "/project/pa/sandbox-api-keys", ""); code < 400 {
		t.Errorf("tenant B lists A's keys: %d %s", code, body)
	}
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &list)

	// Tenant B cannot revoke A's key through its own project.
	if code, _ := f.do(sbxB, http.MethodDelete, "/project/pb/sandbox-api-keys/"+list.Data[0].ID, ""); code != http.StatusOK {
		t.Fatalf("revoke through another project: %d", code)
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes", ""); code != http.StatusOK {
		t.Fatalf("A's key after B's revoke attempt: %d %s", code, body)
	}

	// Removing A from the project stops A's key (403), restoring it brings it back.
	if _, err := f.db.C("project").SetByID(context.Background(), "pa", pgdoc.M{"memberships": []pgdoc.M{}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.C("organization_members").DeleteMany(context.Background(), pgdoc.M{"organizationId": "oa"}); err != nil {
		t.Fatal(err)
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes", ""); code != http.StatusForbidden {
		t.Errorf("key of a removed member: %d %s", code, body)
	}
	mustInsert(t, f.db, "organization_members", pgdoc.M{"organizationId": "oa", "sub": sbxA, "roles": []string{"OWNER"}})

	// Revoked: 401 at once.
	if code, _ := f.do(sbxA, http.MethodDelete, "/project/pa/sandbox-api-keys/"+list.Data[0].ID, ""); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if code, body := f.e2b(keyA, http.MethodGet, "/sandboxes", ""); code != http.StatusUnauthorized {
		t.Errorf("revoked key: %d %s", code, body)
	}
	if code, body := f.e2b(keyB, http.MethodGet, "/sandboxes", ""); code != http.StatusOK {
		t.Errorf("B's key after A's revoke: %d %s", code, body)
	}
}
