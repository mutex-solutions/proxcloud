package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Routes must register without chi panics and every /admin-api/v1 path must match.
func TestRoutes_registerAndMatch(t *testing.T) {
	r := chi.NewRouter()
	(&Handler{}).Routes(r)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/users"}, {http.MethodPost, "/users"},
		{http.MethodGet, "/users/x"}, {http.MethodDelete, "/users/x"},
		{http.MethodGet, "/organizations"}, {http.MethodPost, "/organizations"},
		{http.MethodGet, "/organizations/x"}, {http.MethodPut, "/organizations/x"},
		{http.MethodGet, "/organizations/x/members"}, {http.MethodPost, "/organizations/x/members"},
		{http.MethodDelete, "/organizations/x/members/s"}, {http.MethodPut, "/organizations/x/members/s/role"},
		{http.MethodGet, "/billing_profiles"}, {http.MethodPost, "/billing_profiles"},
		{http.MethodGet, "/billing_profiles/x"}, {http.MethodPut, "/billing_profiles/x"},
		{http.MethodPost, "/billing_profiles/x/activate"},
		{http.MethodPost, "/billing_profiles/x/suspend"},
		{http.MethodPost, "/billing_profiles/x/resume"},
		{http.MethodGet, "/projects"}, {http.MethodPost, "/projects"},
		{http.MethodGet, "/projects/x"}, {http.MethodPost, "/projects/x/provision"},
		{http.MethodGet, "/bills"}, {http.MethodGet, "/bills/x"},
		{http.MethodGet, "/account_credits"}, {http.MethodPost, "/account_credits"},
		{http.MethodGet, "/account_credits/x"}, {http.MethodDelete, "/account_credits/x"},
		{http.MethodGet, "/service_providers"}, {http.MethodGet, "/service_providers/x"},
	} {
		rctx := chi.NewRouteContext()
		if !r.Match(rctx, tc.method, tc.path) {
			t.Errorf("no route for %s %s", tc.method, tc.path)
		}
	}
}

// gate: SigV4 or the admin-api realm passes; anything else 403.
func TestGate(t *testing.T) {
	h := &Handler{apiIssuer: "https://auth/realms/master", apiClientID: "proxcloud-admin-api"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	run := func(rc *httpx.RequestContext) int {
		req := httptest.NewRequest("GET", "/users", nil)
		if rc != nil {
			req = req.WithContext(httpx.WithRC(req.Context(), rc))
		}
		rec := httptest.NewRecorder()
		h.gate(next).ServeHTTP(rec, req)
		return rec.Code
	}
	if got := run(nil); got != 403 {
		t.Errorf("no rc: want 403, got %d", got)
	}
	if got := run(&httpx.RequestContext{SigV4KeyID: "pkX"}); got != 200 {
		t.Errorf("sigv4: want 200, got %d", got)
	}
	if got := run(&httpx.RequestContext{Issuer: "https://auth/realms/master", Azp: "proxcloud-admin-api"}); got != 200 {
		t.Errorf("admin-api bearer: want 200, got %d", got)
	}
	if got := run(&httpx.RequestContext{Issuer: "https://auth/realms/clients", Azp: "proxcloud-ui", Sub: "u1"}); got != 403 {
		t.Errorf("client bearer: want 403, got %d", got)
	}
	// A provider key is filtered out of hmacLookup (purpose != "admin-api"), so it never sets
	// SigV4KeyID — it resolves to an empty credential, which must be denied (finding [3]).
	if got := run(&httpx.RequestContext{}); got != 403 {
		t.Errorf("empty/non-admin credential (filtered provider key): want 403, got %d", got)
	}
}

// Envelopes are snake_case, null fields omitted: list carries next_marker only when a page
// overflows; error is {"error":{"code","message"}}.
func TestEnvelopesAndPaging(t *testing.T) {
	rec := httptest.NewRecorder()
	items, next := pageOut(listReq{Limit: 2}, []apiUser{{ID: "1"}, {ID: "2"}, {ID: "3"}}, func(u apiUser) string { return u.ID })
	writeList(rec, items, next)
	var lst struct {
		Data       []map[string]any `json:"data"`
		NextMarker string           `json:"next_marker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lst); err != nil {
		t.Fatal(err)
	}
	if len(lst.Data) != 2 || lst.NextMarker != "3" {
		t.Fatalf("page: %+v", lst)
	}
	// Under-full page → no next_marker key at all.
	rec2 := httptest.NewRecorder()
	items2, next2 := pageOut(listReq{Limit: 2}, []apiUser{{ID: "1"}}, func(u apiUser) string { return u.ID })
	writeList(rec2, items2, next2)
	if strings.Contains(rec2.Body.String(), "next_marker") {
		t.Fatalf("unexpected next_marker: %s", rec2.Body.String())
	}
	rec3 := httptest.NewRecorder()
	apiNotFound(rec3)
	if rec3.Code != 404 || rec3.Body.String() != `{"error":{"code":"NOT_FOUND","message":"Not Found"}}`+"\n" {
		t.Fatalf("error envelope: %d %s", rec3.Code, rec3.Body.String())
	}
}
