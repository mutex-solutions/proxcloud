//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestAdminProjectDeletionLegs guards wave-4: DELETE /admin/project/{id} runs the CanDelete pre-check
// then flips SCHEDULED_FOR_DELETION; DELETE /admin/project/{id}/now flips DELETE_IN_PROGRESS and
// dispatches the Teardown leg. Without the cloud legs wired, both stay 501 (the unit-test posture).
func TestAdminProjectDeletionLegs(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	reseed := func() {
		_, _ = db.C("project").DeleteMany(ctx, pgdoc.M{})
		_, _ = db.C("project").InsertOne(ctx, pgdoc.M{"_id": "proj-1", "status": "ENABLED"})
	}
	const iss, cid = "test-iss", "test-cid"
	status := func() string {
		var p pgdoc.M
		_, _ = db.C("project").FindOne(ctx, pgdoc.M{"_id": "proj-1"}, &p)
		s, _ := p["status"].(string)
		return s
	}
	newH := func(ops *admin.ProjectCloudOps) http.Handler {
		h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, nil, "", iss, cid)
		if ops != nil {
			h.SetProjectCloudOps(ops)
		}
		r := chi.NewRouter()
		h.Routes(r)
		return r
	}
	do := func(handler http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	// unwired → 501 (no state change)
	reseed()
	if w := do(newH(nil), "/admin/project/proj-1"); w.Code != http.StatusNotImplemented {
		t.Fatalf("unwired schedule want 501, got %d", w.Code)
	}
	if status() != "ENABLED" {
		t.Fatalf("unwired schedule must not flip status, got %s", status())
	}

	// wired
	var teardownFor string
	wired := newH(&admin.ProjectCloudOps{
		CanDelete: func(context.Context, string) error { return nil },
		Teardown:  func(_ context.Context, pid string) error { teardownFor = pid; return nil },
	})

	reseed()
	if w := do(wired, "/admin/project/proj-1"); w.Code != 200 {
		t.Fatalf("schedule: status=%d body=%s", w.Code, w.Body)
	}
	if status() != "SCHEDULED_FOR_DELETION" {
		t.Fatalf("schedule should flip SCHEDULED_FOR_DELETION, got %s", status())
	}

	if w := do(wired, "/admin/project/proj-1/now"); w.Code != 200 {
		t.Fatalf("deleteNow: status=%d body=%s", w.Code, w.Body)
	}
	if status() != "DELETE_IN_PROGRESS" {
		t.Fatalf("deleteNow should flip DELETE_IN_PROGRESS, got %s", status())
	}
	if teardownFor != "proj-1" {
		t.Fatalf("teardown leg should fire for proj-1, got %q", teardownFor)
	}
}
