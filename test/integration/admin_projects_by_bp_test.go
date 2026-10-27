//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// GET /admin/project/{bpId}/billing-profile must list the projects that BILL against the profile
// under the effective-profile rule (project's own billingProfileId, falling back to the owning
// org's): greenfield projects carry a BLANK own id, so the org-fallback leg is what makes the
// admin billing-profile Projects tab non-empty.
func TestAdminProjectsByBillingProfile_EffectiveFallback(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)

	const bpID, otherBP, orgID = "bp-1", "bp-other", "org-1"
	if _, err := db.C("organization").InsertOne(ctx, pgdoc.M{"_id": orgID, "billingProfileId": bpID}); err != nil {
		t.Fatal(err)
	}
	seed := func(id string, doc pgdoc.M) {
		doc["_id"] = id
		if _, err := db.C("project").InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	seed("p-direct", pgdoc.M{"billingProfileId": bpID})                                // own id matches
	seed("p-org", pgdoc.M{"organizationId": orgID})                                    // blank own id → bills via the org
	seed("p-elsewhere", pgdoc.M{"organizationId": orgID, "billingProfileId": otherBP}) // explicitly billed elsewhere
	seed("p-unrelated", pgdoc.M{"organizationId": "org-x"})

	const iss, cid = "test-iss", "test-cid"
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, nil, "", iss, cid)
	r := chi.NewRouter()
	h.Routes(r)

	req := httptest.NewRequest(http.MethodGet, "/admin/project/"+bpID+"/billing-profile", nil)
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	var env struct {
		Data []pgdoc.M `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]bool{}
	for _, p := range env.Data {
		id, _ := p["_id"].(string)
		got[id] = true
	}
	if len(env.Data) != 2 || !got["p-direct"] || !got["p-org"] {
		t.Fatalf("want exactly [p-direct p-org], got %v", got)
	}
}
