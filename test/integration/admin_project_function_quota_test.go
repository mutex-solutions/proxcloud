//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestAdminProjectFunctionQuota: the operator's function quota of a project is a document of its own. Writing it
// leaves the project's quota document, its status, its bindings and its members byte for byte as they were; the quota
// route in turn refuses a function limit and leaves the function quota alone; and the function quota a sandbox
// provider resolves for the project follows the write, key by key, never unlimited.
//
// The case that makes it matter: a project whose quota document holds only sandbox keys is no Proxmox quota, so it
// gets the provider's Proxmox defaults. One more key in that document, such as a function limit, would turn it into
// a Proxmox quota in which servers, vCPUs, memory and disk are unlimited.
func TestAdminProjectFunctionQuota(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	projects := project.NewService(project.NewRepo(db), nil, nil, nil, nil)
	const iss, cid = "test-iss", "test-cid"
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, esSvc, "r1", iss, cid)
	router := chi.NewRouter()
	router.MethodNotAllowed(httpx.MethodNotAllowedHandler())
	h.Routes(router)

	mustInsert(t, db, "externalService", pgdoc.M{"_id": "sbx", "name": "sbx", "type": externalservice.TypeCloud, "status": externalservice.StatusDisabled,
		"config": pgdoc.M{"provider": "sandbox", "sandbox": pgdoc.M{"domain": "sbx.example.org",
			"functions": pgdoc.M{"enabled": false, "projectQuota": pgdoc.M{"vcpus": 4}}}}})
	mustInsert(t, db, "project", pgdoc.M{"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
		"quota":    pgdoc.M{"sandboxes": 2, "sandboxVcpus": 4},
		"services": []any{pgdoc.M{"serviceId": "sbx", "provider": "sandbox", "region": "r1"}}})

	put := func(path, body string) (int, string) {
		req := httptest.NewRequest(http.MethodPut, "/admin/project/p1/"+path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	stored := func() pgdoc.M {
		t.Helper()
		var doc pgdoc.M
		if found, err := db.C("project").Get(ctx, "p1", &doc); err != nil || !found {
			t.Fatalf("project: %v %v", found, err)
		}
		return doc
	}
	// rest is the project document without the function quota and the id the store adds.
	rest := func() pgdoc.M {
		doc := stored()
		delete(doc, "functionQuota")
		return doc
	}
	resolved := func() string {
		t.Helper()
		p, err := projects.GetProjectByID(ctx, "p1")
		if err != nil {
			t.Fatal(err)
		}
		es, err := esSvc.Get(ctx, "sbx")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%+v", es.FunctionProjectQuota(p.FunctionQuota))
	}
	before := rest()
	if got := resolved(); got != "{Functions:20 VCPUs:4 RAMMb:16384}" {
		t.Fatalf("a project without a function quota: %s", got)
	}

	// The write: only the one field, and what a provider resolves follows it.
	code, body := put("function-quota", `{"functions":3,"ramMb":2048}`)
	if code != http.StatusOK || !strings.Contains(body, `"functionQuota":{"functions":3,"ramMb":2048}`) {
		t.Fatalf("PUT function-quota: %d %s", code, body)
	}
	if got := rest(); !reflect.DeepEqual(got, before) {
		t.Fatalf("the function quota write changed the rest of the project:\nbefore %v\nafter  %v", before, got)
	}
	if got := resolved(); got != "{Functions:3 VCPUs:4 RAMMb:2048}" {
		t.Fatalf("after the write: %s", got)
	}
	// A whole replace, not a merge: a key left out is the provider's again.
	if code, body = put("function-quota", `{"vcpus":0}`); code != http.StatusOK || resolved() != "{Functions:20 VCPUs:0 RAMMb:16384}" {
		t.Fatalf("PUT function-quota with one key: %d %s, resolves to %s", code, body, resolved())
	}

	// Refused bodies store nothing.
	for _, bad := range []string{`{"vcpus":-1}`, `{"ramMb":1.5}`, `{"functions":"3"}`, `{"servers":5}`, `{"sandboxVcpus":2}`, `{"functionVcpus":2}`, `[1]`} {
		if code, body = put("function-quota", bad); code != http.StatusBadRequest {
			t.Errorf("PUT function-quota %s: %d %s, want 400", bad, code, body)
		}
	}
	if got := resolved(); got != "{Functions:20 VCPUs:0 RAMMb:16384}" || !reflect.DeepEqual(rest(), before) {
		t.Fatalf("a refused body changed the project: %s", got)
	}

	// The quota route refuses a function limit in every spelling, and leaves both documents as they are.
	for _, key := range []string{"functions", "functionVcpus", "functionRamMb", "functionQuota"} {
		if code, body = put("quota", `{"sandboxes":2,"sandboxVcpus":4,"`+key+`":5}`); code != http.StatusBadRequest || !strings.Contains(body, "function-quota") {
			t.Errorf("PUT quota with %s: %d %s, want 400", key, code, body)
		}
	}
	if !reflect.DeepEqual(rest(), before) || resolved() != "{Functions:20 VCPUs:0 RAMMb:16384}" {
		t.Fatalf("a refused quota write changed the project: %v", stored())
	}
	// A quota write that is taken does not touch the function quota.
	if code, body = put("quota", `{"sandboxes":3}`); code != http.StatusOK {
		t.Fatalf("PUT quota: %d %s", code, body)
	}
	doc := stored()
	if quota, _ := doc["quota"].(map[string]any); len(quota) != 1 || fmt.Sprint(quota["sandboxes"]) != "3" || resolved() != "{Functions:20 VCPUs:0 RAMMb:16384}" {
		t.Fatalf("after a quota write: quota %v, function quota resolves to %s", doc["quota"], resolved())
	}
	// The quota document still holds only sandbox keys: no Proxmox quota, so no Proxmox limit was lifted.
	for key := range doc["quota"].(map[string]any) {
		found := false
		for _, sandboxKey := range externalservice.SandboxQuotaKeys {
			found = found || key == sandboxKey
		}
		if !found {
			t.Errorf("the project's quota document gained the key %q", key)
		}
	}

	// An empty body removes the function quota; the quota document stays.
	if code, body = put("function-quota", `{}`); code != http.StatusOK || strings.Contains(body, "functionQuota") {
		t.Fatalf("PUT function-quota {}: %d %s", code, body)
	}
	if doc = stored(); doc["functionQuota"] != nil || fmt.Sprint(doc["quota"].(map[string]any)["sandboxes"]) != "3" || resolved() != "{Functions:20 VCPUs:4 RAMMb:16384}" {
		t.Fatalf("after clearing: %v, resolves to %s", doc, resolved())
	}
	if code, _ = put("function-quota", `{"vcpus":1}`); code != http.StatusOK {
		t.Fatal(code)
	}
	req := httptest.NewRequest(http.MethodPut, "/admin/project/nope/function-quota", strings.NewReader(`{"vcpus":1}`))
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
	w := httptest.NewRecorder()
	if router.ServeHTTP(w, req); w.Code != http.StatusNotFound {
		t.Errorf("an unknown project: %d, want 404", w.Code)
	}
}
