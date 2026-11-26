//go:build integration

package integration

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestAdminProjectDeletionLegs guards wave-4: DELETE /admin/project/{id} runs the CanDelete pre-check
// then flips SCHEDULED_FOR_DELETION; DELETE /admin/project/{id}/now flags DELETE_IN_PROGRESS where the
// deletion job looks. Without the cloud legs wired, both stay 501 (the unit-test posture).
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
	auditSvc := audit.NewService(audit.NewRepo(db), slog.Default())
	newH := func(ops *admin.ProjectCloudOps) http.Handler {
		h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, auditSvc, nil, "", iss, cid)
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

	// A pre-check that fails with a plain error (the cloud unreachable) answers 500 and schedules nothing.
	reseed()
	cantCheck := newH(&admin.ProjectCloudOps{CanDelete: func(context.Context, string) error { return errors.New("cloud down") }})
	if w := do(cantCheck, "/admin/project/proj-1"); w.Code != http.StatusInternalServerError || status() != "ENABLED" {
		t.Fatalf("schedule on a failing pre-check: status=%d project=%s", w.Code, status())
	}

	// wired
	repo := project.NewRepo(db)
	svc := project.NewService(repo, nil, nil, nil, nil)

	// Without the deletion job (scheduler and debug triggers off) a scheduled delete is refused as a delete-now is, with
	// the same reason, and nothing is written: no job would ever carry it out (drill R4-o2 of v0.11.1). The pre-check
	// is the one cmd/api wires.
	reseed()
	noJob := newH(&admin.ProjectCloudOps{
		CanDelete: svc.CanScheduleDeletion,
		DeleteNow: func(ctx context.Context, pid string) error { _, err := svc.DeleteNow(ctx, pid); return err },
	})
	svc.SetNoDeletionJob(true)
	scheduled, now := do(noJob, "/admin/project/proj-1"), do(noJob, "/admin/project/proj-1/now")
	if scheduled.Code != http.StatusConflict || !strings.Contains(scheduled.Body.String(), "scheduler is off") || status() != "ENABLED" {
		t.Fatalf("schedule without the deletion job: status=%d project=%s body=%s", scheduled.Code, status(), scheduled.Body)
	}
	if now.Code != http.StatusConflict || scheduled.Body.String() != now.Body.String() {
		t.Fatalf("the two routes give two answers:\nschedule %d %s\nnow      %d %s", scheduled.Code, scheduled.Body, now.Code, now.Body)
	}
	// With the job the same pre-check lets the project through, and refuses one that does not resolve.
	svc.SetNoDeletionJob(false)
	if w := do(noJob, "/admin/project/proj-1"); w.Code != 200 || status() != "SCHEDULED_FOR_DELETION" {
		t.Fatalf("schedule with the deletion job: status=%d project=%s body=%s", w.Code, status(), w.Body)
	}
	if err := svc.CanScheduleDeletion(ctx, "no-such-project"); err == nil {
		t.Fatal("a project that does not resolve passed the pre-check")
	}
	wired := newH(&admin.ProjectCloudOps{
		CanDelete: func(context.Context, string) error { return nil },
		DeleteNow: func(ctx context.Context, pid string) error { _, err := svc.DeleteNow(ctx, pid); return err },
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
	// The audit log tells an immediate delete from a scheduled one by the status it recorded.
	var nowEvent bool
	for deadline := time.Now().Add(5 * time.Second); !nowEvent && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		evs, err := auditSvc.QueryAll(ctx, audit.Filter{}, 50)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			for _, c := range ev.Changes {
				if ev.Action == audit.ActionDelete && ev.ResourceID == "proj-1" && c.Field == "status" && c.NewValue == "DELETE_IN_PROGRESS" {
					nowEvent = true
				}
			}
		}
	}
	if !nowEvent {
		t.Fatal("deleteNow wrote no audit event with its status flip")
	}

	// A failure underneath (here the save) answers 500, never 200.
	reseed()
	failing := newH(&admin.ProjectCloudOps{
		CanDelete: func(context.Context, string) error { return nil },
		DeleteNow: func(context.Context, string) error { return errors.New("database down") },
	})
	if w := do(failing, "/admin/project/proj-1/now"); w.Code != http.StatusInternalServerError {
		t.Fatalf("deleteNow on a failing save: status=%d body=%s", w.Code, w.Body)
	}
	if w := do(wired, "/admin/project/proj-1/now"); w.Code != 200 || status() != "DELETE_IN_PROGRESS" {
		t.Fatalf("deleteNow again: status=%d project=%s", w.Code, status())
	}

	// Nothing runs in the request, so a restart or a failed pass cannot strand it: the job retries until a clean pass.
	deleter := &flakyDeleter{fails: 1}
	job := project.NewDeletionJob(repo, deleter, nil)
	if n, err := job.ExecuteAll(ctx); err != nil || n != 0 {
		t.Fatalf("failed pass: n=%d err=%v", n, err)
	}
	if status() != "DELETE_IN_PROGRESS" {
		t.Fatalf("a failed pass must stay in progress, got %s", status())
	}
	if n, err := job.ExecuteAll(ctx); err != nil || n != 1 || deleter.calls != 2 {
		t.Fatalf("retry: n=%d err=%v calls=%d", n, err, deleter.calls)
	}
	if status() != "" {
		t.Fatalf("a clean pass removes the project, got %s", status())
	}

	// A row flagged by the old admin delete-now has no schedule, so the sweep never selects it: delete-now must still take
	// it, and the sweep then finishes it. One the sweep is retrying (it has a schedule) stays refused.
	_, _ = db.C("project").DeleteMany(ctx, pgdoc.M{})
	_, _ = db.C("project").InsertOne(ctx, pgdoc.M{"_id": "proj-1", "status": "DELETE_IN_PROGRESS"})
	if n, err := job.ExecuteAll(ctx); err != nil || n != 0 {
		t.Fatalf("a stranded row is not the sweep's: n=%d err=%v", n, err)
	}
	if w := do(wired, "/admin/project/proj-1/now"); w.Code != 200 {
		t.Fatalf("deleteNow of a stranded row: status=%d body=%s", w.Code, w.Body)
	}
	if w := do(wired, "/admin/project/proj-1/now"); w.Code != http.StatusBadRequest {
		t.Fatalf("deleteNow of a row the sweep retries: status=%d body=%s", w.Code, w.Body)
	}
	if n, err := job.ExecuteAll(ctx); err != nil || n != 1 || status() != "" {
		t.Fatalf("sweep after re-flag: n=%d err=%v status=%s", n, err, status())
	}

	// Past the point of no return, neither cancel may bring it back.
	for _, st := range []string{"DELETE_IN_PROGRESS", "DELETED"} {
		_, _ = db.C("project").DeleteMany(ctx, pgdoc.M{})
		_, _ = db.C("project").InsertOne(ctx, pgdoc.M{"_id": "proj-1", "status": st})
		if w := do(wired, "/admin/project/proj-1/cancel"); w.Code != http.StatusBadRequest {
			t.Fatalf("admin cancel of %s: status=%d", st, w.Code)
		}
		if _, err := svc.CancelDeletion(ctx, "proj-1"); err == nil {
			t.Fatalf("client cancel of %s must be refused", st)
		}
		if status() != st {
			t.Fatalf("cancel flipped %s to %s", st, status())
		}
	}
}

type flakyDeleter struct{ fails, calls int }

func (d *flakyDeleter) DeleteProjectResources(context.Context, string) error {
	d.calls++
	if d.calls <= d.fails {
		return errors.New("upstream down")
	}
	return nil
}
