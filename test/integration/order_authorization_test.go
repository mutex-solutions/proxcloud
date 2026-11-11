//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/order"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Exercise the production order -> billing profile -> organization -> RBAC read
// gate over real repositories. Membership alone is not a billing-read grant, and
// denied reads must not reveal whether an order or its ownership links exist.
func TestOrderReadAuthorization(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	orgRepo := org.NewRepo(db)
	if err := orgRepo.EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure org indexes: %v", err)
	}
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	orders := order.NewRepo(db)
	billingH := org.NewBillingHandler(org.NewService(orgRepo, billingRepo, nil), org.NewPolicy(orgRepo),
		billingRepo, nil, nil, nil, nil, nil, "", users, orders)
	router := chi.NewRouter()
	order.NewHandler(orders, users, billingH.CanReadBillingProfile).Routes(router)

	const bpID = "order-read-profile"
	o, err := orgRepo.Insert(ctx, &org.Organization{
		Name: "Order owner", BillingProfileID: bpID, CustomInfo: map[string]any{},
	})
	if err != nil {
		t.Fatalf("insert org: %v", err)
	}
	mustInsert(t, db, "billingProfile", pgdoc.M{
		"_id": bpID, "organizationId": o.ID, "currency": "USD",
	})
	const restrictedRole = "order-restricted"
	const readRole = "order-reader"
	if _, err := orgRepo.InsertRole(ctx, &org.Role{
		OrganizationID: o.ID, Name: restrictedRole,
		Permissions: []string{"organization:read", rbac.BillingProfileReadInvoices},
	}); err != nil {
		t.Fatalf("insert restricted role: %v", err)
	}
	readerRole, err := orgRepo.InsertRole(ctx, &org.Role{
		OrganizationID: o.ID, Name: readRole, Permissions: []string{rbac.BillingProfileRead},
	})
	if err != nil {
		t.Fatalf("insert reader role: %v", err)
	}
	for _, m := range []struct{ sub, role string }{
		{"order-owner", rbac.RoleOwner},
		{"order-member", rbac.RoleMember},
		{"order-restricted", restrictedRole},
		{"order-reader", readRole},
	} {
		mustInsert(t, db, "users", pgdoc.M{"sub": m.sub, "email": m.sub + "@example.test"})
		if _, err := orgRepo.AddMember(ctx, o.ID, m.sub, m.role); err != nil {
			t.Fatalf("add %s: %v", m.sub, err)
		}
	}
	// An owner in another organization still has no access to this order.
	otherOrg, err := orgRepo.Insert(ctx, &org.Organization{Name: "Other org", CustomInfo: map[string]any{}})
	if err != nil {
		t.Fatalf("insert other org: %v", err)
	}
	mustInsert(t, db, "users", pgdoc.M{"sub": "order-outsider", "email": "outsider@example.test"})
	if _, err := orgRepo.AddMember(ctx, otherOrg.ID, "order-outsider", rbac.RoleOwner); err != nil {
		t.Fatalf("add outsider to own org: %v", err)
	}

	const orderID = "order-read-existing"
	net, tax := decimalOf(t, "12.34"), decimalOf(t, "1.23")
	createdAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	mustInsert(t, db, "order", pgdoc.M{
		"_id": orderID, "billingProfileId": bpID,
		"netAmount": net, "taxAmount": tax, "status": order.OrderStatusCreated,
		"createdAt": createdAt, "updatedAt": createdAt,
		"items": []order.OrderItem{{
			Name: "Compute", Type: "instance", NetAmount: &net, TaxAmount: &tax,
			ResourceID: "resource-1", Metadata: map[string]any{"region": "test-region"},
		}},
		// Unknown stored fields must not leak through an untyped/raw order read.
		"internalOnly": "not part of the order DTO",
	})

	get := func(h http.Handler, sub, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/orders/"+id, nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	assertDTO := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		if got := w.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("content type=%q", got)
		}
		var response httpx.Response[order.Order]
		decoder := json.NewDecoder(w.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&response); err != nil {
			t.Fatalf("decode typed order DTO: %v", err)
		}
		got := response.Data
		if response.Errors != nil || got == nil {
			t.Fatalf("expected data without errors: %+v", response)
		}
		if got.ID != orderID || got.BillingProfileID != bpID || got.Status != order.OrderStatusCreated {
			t.Fatalf("order identity/status: %+v", got)
		}
		if got.NetAmount == nil || !got.NetAmount.Equal(net) || got.TaxAmount == nil || !got.TaxAmount.Equal(tax) {
			t.Fatalf("order amounts: %+v", got)
		}
		if got.CreatedAt == nil || !got.CreatedAt.Equal(createdAt) || got.UpdatedAt == nil || !got.UpdatedAt.Equal(createdAt) {
			t.Fatalf("order timestamps: %+v", got)
		}
		if len(got.Items) != 1 {
			t.Fatalf("items=%+v", got.Items)
		}
		item := got.Items[0]
		if item.Name != "Compute" || item.Type != "instance" || item.ResourceID != "resource-1" ||
			item.NetAmount == nil || !item.NetAmount.Equal(net) || item.TaxAmount == nil || !item.TaxAmount.Equal(tax) ||
			item.Metadata["region"] != "test-region" {
			t.Fatalf("order item: %+v", item)
		}
	}
	assertNotFound := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		var response httpx.Response[order.Order]
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode 404: %v", err)
		}
		if response.Data != nil || response.Errors == nil || response.Errors.Code != http.StatusNotFound || response.Errors.Message != "Order not found" {
			t.Fatalf("expected opaque order 404: %s", w.Body)
		}
	}
	assertHidden := func(t *testing.T, h http.Handler, sub, id string) {
		t.Helper()
		missing := get(h, sub, "order-read-nonexistent")
		denied := get(h, sub, id)
		assertNotFound(t, missing)
		assertNotFound(t, denied)
		if denied.Body.String() != missing.Body.String() || denied.Header().Get("Content-Type") != missing.Header().Get("Content-Type") {
			t.Fatalf("denied order differs from missing: denied=%s missing=%s", denied.Body, missing.Body)
		}
	}

	for _, sub := range []string{"order-owner", "order-member", "order-reader"} {
		t.Run(sub+" succeeds with typed DTO", func(t *testing.T) {
			assertDTO(t, get(router, sub, orderID))
		})
	}
	for _, sub := range []string{"order-outsider", "order-restricted"} {
		t.Run(sub+" cannot distinguish existing order", func(t *testing.T) {
			assertHidden(t, router, sub, orderID)
		})
	}

	t.Run("broken ownership links fail closed", func(t *testing.T) {
		// Retain an owner's membership after deleting the org: membership without
		// a live owning organization must not authorize a dangling profile.
		deletedOrg, err := orgRepo.Insert(ctx, &org.Organization{Name: "Deleted org", CustomInfo: map[string]any{}})
		if err != nil {
			t.Fatalf("insert deleted org: %v", err)
		}
		if _, err := orgRepo.AddMember(ctx, deletedOrg.ID, "order-owner", rbac.RoleOwner); err != nil {
			t.Fatalf("add deleted org owner: %v", err)
		}
		mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "profile-deleted-org", "organizationId": deletedOrg.ID, "currency": "USD"})
		if err := orgRepo.Delete(ctx, deletedOrg.ID); err != nil {
			t.Fatalf("delete org: %v", err)
		}
		mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "profile-no-org", "currency": "USD"})
		mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "profile-missing-org", "organizationId": "missing-org", "currency": "USD"})
		mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "profile-deleted", "organizationId": o.ID, "currency": "USD"})
		if _, err := db.C("billingProfile").DeleteByID(ctx, "profile-deleted"); err != nil {
			t.Fatalf("delete profile: %v", err)
		}
		for _, tc := range []struct{ name, profile string }{
			{"blank profile", ""},
			{"missing profile", "profile-nonexistent"},
			{"deleted profile", "profile-deleted"},
			{"profile without org", "profile-no-org"},
			{"profile with missing org", "profile-missing-org"},
			{"profile with deleted org", "profile-deleted-org"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id := "broken-" + tc.profile
				mustInsert(t, db, "order", pgdoc.M{"_id": id, "billingProfileId": tc.profile, "status": order.OrderStatusCreated})
				assertHidden(t, router, "order-owner", id)
			})
		}
		mustInsert(t, db, "order", pgdoc.M{"_id": "order-without-profile", "status": order.OrderStatusCreated})
		assertHidden(t, router, "order-owner", "order-without-profile")
	})

	t.Run("nil authorizer fails closed even for owner", func(t *testing.T) {
		unwired := chi.NewRouter()
		order.NewHandler(orders, users, nil).Routes(unwired)
		assertHidden(t, unwired, "order-owner", orderID)
	})

	t.Run("permission and membership role revocation affect next read", func(t *testing.T) {
		assertDTO(t, get(router, "order-reader", orderID))
		readerRole.Permissions = []string{"organization:read"}
		if err := orgRepo.SaveRole(ctx, readerRole); err != nil {
			t.Fatalf("revoke custom permission: %v", err)
		}
		assertHidden(t, router, "order-reader", orderID)
		readerRole.Permissions = []string{rbac.BillingProfileRead}
		if err := orgRepo.SaveRole(ctx, readerRole); err != nil {
			t.Fatalf("restore custom permission: %v", err)
		}
		assertDTO(t, get(router, "order-reader", orderID))
		if err := orgRepo.UpdateMemberRole(ctx, o.ID, "order-reader", restrictedRole); err != nil {
			t.Fatalf("revoke member role: %v", err)
		}
		assertHidden(t, router, "order-reader", orderID)
	})
}
