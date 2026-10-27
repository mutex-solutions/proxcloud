//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestAdminOrganizationCreateAutoBillingProfile guards wave-2's org-auto-bp leg: creating an
// organization with createBillingProfile:true must persist the org, add the owner as OWNER, and
// create + link an owner-populated BillingProfile (StatusNew) — previously a 501. Also checks the
// owner-required validation stays 400.
func TestAdminOrganizationCreateAutoBillingProfile(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	if _, err := db.C("users").InsertOne(ctx, pgdoc.M{
		"_id": "6a4a00000000000000000001", "sub": "owner-sub", "email": "owner@x.io", "firstName": "Own", "lastName": "Er",
	}); err != nil {
		t.Fatal(err)
	}

	const iss, cid = "test-iss", "test-cid"
	h := admin.NewHandler(admin.NewRepo(db), nil, user.NewRepo(db), nil, nil, billing.NewRepo(db), nil, nil, nil, nil, "", iss, cid)
	r := chi.NewRouter()
	h.Routes(r)
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/organizations", strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// createBillingProfile without owner → 400 (validation preserved)
	if w := do(`{"name":"NoOwner","createBillingProfile":true}`); w.Code != 400 {
		t.Fatalf("createBP without owner want 400, got %d", w.Code)
	}

	// happy path → 200 + org + bp + owner member
	if w := do(`{"name":"Acme","ownerSub":"owner-sub","createBillingProfile":true}`); w.Code != 200 {
		t.Fatalf("create auto-bp: status=%d body=%s", w.Code, w.Body)
	}

	var org pgdoc.M
	if found, err := db.C("organization").FindOne(ctx, pgdoc.M{"name": "Acme"}, &org); err != nil || !found {
		t.Fatalf("org not created: found=%v err=%v", found, err)
	}
	bpID, _ := org["billingProfileId"].(string)
	if bpID == "" {
		t.Fatalf("org.billingProfileId not set: %+v", org)
	}
	orgID, _ := org["_id"].(string)
	if orgID == "" {
		t.Fatalf("unexpected _id: %+v", org["_id"])
	}

	var bp pgdoc.M
	if found, err := db.C("billingProfile").FindOne(ctx, pgdoc.M{"organizationId": orgID}, &bp); err != nil || !found {
		t.Fatalf("billingProfile not created for org %s: found=%v err=%v", orgID, found, err)
	}
	if bp["email"] != "owner@x.io" || bp["status"] != "NEW" {
		t.Fatalf("bp not owner-populated/NEW: %+v", bp)
	}

	n, _ := db.C("organization_members").Count(ctx, pgdoc.M{"organizationId": orgID, "sub": "owner-sub"})
	if n != 1 {
		t.Fatalf("owner should be a member of the org, got %d", n)
	}
}
