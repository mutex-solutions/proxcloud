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
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestProjectStatusMachine drives the client service and the admin routes against stored docs: a cancel restores what was
// held and never lifts a suspension, and nothing moves a project in teardown.
func TestProjectStatusMachine(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := project.NewRepo(db)
	svc := project.NewService(repo, nil, nil, nil, nil)
	const iss, cid = "test-iss", "test-cid"
	var paused []bool
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, nil, "", iss, cid)
	h.SetProjectCloudOps(&admin.ProjectCloudOps{
		PauseServers: func(_ context.Context, _ string, pause bool) error { paused = append(paused, pause); return nil },
		CanDelete:    func(context.Context, string) error { return nil },
		DeleteNow:    func(ctx context.Context, id string) error { _, err := svc.DeleteNow(ctx, id); return err },
	})
	router := chi.NewRouter()
	h.Routes(router)
	do := func(method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	seed := func(doc pgdoc.M) {
		_, _ = db.C("project").DeleteMany(ctx, pgdoc.M{})
		doc["_id"] = "p1"
		if _, err := db.C("project").InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() project.Project {
		p, err := repo.FindByID(ctx, "p1")
		if err != nil || p == nil {
			t.Fatalf("read back: %v %v", p, err)
		}
		return *p
	}

	t.Run("client cancel restores the held status", func(t *testing.T) {
		for _, held := range []string{project.StatusEnabled, project.StatusDisabled} {
			seed(pgdoc.M{"status": held})
			p := stored()
			if _, err := svc.ScheduleDeletion(ctx, &p, false); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CancelDeletion(ctx, "p1"); err != nil {
				t.Fatal(err)
			}
			got := stored()
			if got.Status != held || got.StatusBeforeDeletion != "" || got.ScheduledForDeletionAt != nil {
				t.Fatalf("held %s: %+v", held, got)
			}
		}
	})

	t.Run("client cancel needs a scheduled project", func(t *testing.T) {
		for _, st := range []string{project.StatusDisabled, project.StatusEnabled, project.StatusDeleteInProgress, project.StatusDeleted} {
			seed(pgdoc.M{"status": st})
			if _, err := svc.CancelDeletion(ctx, "p1"); err == nil {
				t.Fatalf("cancel of %s must be refused", st)
			}
			if got := stored().Status; got != st {
				t.Fatalf("cancel flipped %s to %s", st, got)
			}
		}
	})

	t.Run("a schedule from before the field cancels to DISABLED", func(t *testing.T) {
		seed(pgdoc.M{"status": project.StatusScheduledForDeletion})
		if _, err := svc.CancelDeletion(ctx, "p1"); err != nil {
			t.Fatal(err)
		}
		if got := stored().Status; got != project.StatusDisabled {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("client schedule and delete-now refuse what is going or gone", func(t *testing.T) {
		for _, st := range []string{project.StatusDeleteInProgress, project.StatusDeleted} {
			seed(pgdoc.M{"status": st})
			p := stored()
			if _, err := svc.ScheduleDeletion(ctx, &p, false); err == nil {
				t.Fatalf("schedule of %s must be refused", st)
			}
			if _, err := svc.DeleteNow(ctx, "p1"); err == nil {
				t.Fatalf("delete-now of %s must be refused", st)
			}
			if got := stored().Status; got != st {
				t.Fatalf("flipped %s to %s", st, got)
			}
		}
	})

	t.Run("a write decided on a stale read loses", func(t *testing.T) {
		seed(pgdoc.M{"status": project.StatusScheduledForDeletion})
		p := stored()
		p.Status = project.StatusEnabled
		seed(pgdoc.M{"status": project.StatusDeleteInProgress})
		if ok, err := repo.SaveIfStatus(ctx, &p, project.StatusScheduledForDeletion); err != nil || ok {
			t.Fatalf("ok=%v err=%v", ok, err)
		}
		if got := stored().Status; got != project.StatusDeleteInProgress {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("admin status route", func(t *testing.T) {
		cases := []struct {
			from, to string
			code     int
			want     string
		}{
			{project.StatusDisabled, "ENABLED", 200, project.StatusEnabled},
			{project.StatusEnabled, "DISABLED", 200, project.StatusDisabled},
			{project.StatusEnabled, "ENABLED", 400, project.StatusEnabled},
			{project.StatusScheduledForDeletion, "ENABLED", 400, project.StatusScheduledForDeletion},
			{project.StatusScheduledForDeletion, "DISABLED", 400, project.StatusScheduledForDeletion},
			{project.StatusDeleteInProgress, "ENABLED", 400, project.StatusDeleteInProgress},
			{project.StatusDeleteInProgress, "DISABLED", 400, project.StatusDeleteInProgress},
			{project.StatusDeleted, "ENABLED", 400, project.StatusDeleted},
			{project.StatusEnabled, "DELETE_IN_PROGRESS", 400, project.StatusEnabled},
		}
		for _, c := range cases {
			seed(pgdoc.M{"status": c.from})
			paused = nil
			if code := do(http.MethodPost, "/admin/project/p1/"+c.to); code != c.code {
				t.Errorf("%s -> %s: status %d, want %d", c.from, c.to, code, c.code)
			}
			if got := stored().Status; got != c.want {
				t.Errorf("%s -> %s: stored %s, want %s", c.from, c.to, got, c.want)
			}
			if c.code != 200 && len(paused) != 0 {
				t.Errorf("%s -> %s: a refused change touched the VMs", c.from, c.to)
			}
		}
		seed(pgdoc.M{"status": project.StatusEnabled})
		do(http.MethodPost, "/admin/project/p1/DISABLED")
		if !stored().DisabledByOperator {
			t.Error("an operator's disable must be marked")
		}
		do(http.MethodPost, "/admin/project/p1/ENABLED")
		if stored().DisabledByOperator {
			t.Error("an operator's enable must clear the mark")
		}
	})

	t.Run("admin schedule and cancel", func(t *testing.T) {
		seed(pgdoc.M{"status": project.StatusDisabled})
		if code := do(http.MethodDelete, "/admin/project/p1"); code != 200 {
			t.Fatalf("schedule: %d", code)
		}
		if code := do(http.MethodDelete, "/admin/project/p1/cancel"); code != 200 {
			t.Fatalf("cancel: %d", code)
		}
		if got := stored().Status; got != project.StatusDisabled {
			t.Fatalf("cancel restored %s", got)
		}
		if code := do(http.MethodDelete, "/admin/project/p1/cancel"); code != 400 {
			t.Fatalf("cancel of a DISABLED project: %d", code)
		}
		for _, st := range []string{project.StatusDeleteInProgress, project.StatusDeleted} {
			seed(pgdoc.M{"status": st})
			if code := do(http.MethodDelete, "/admin/project/p1"); code != 400 {
				t.Errorf("schedule of %s: %d", st, code)
			}
			if code := do(http.MethodDelete, "/admin/project/p1/now"); code != 400 {
				t.Errorf("delete-now of %s: %d", st, code)
			}
			if got := stored().Status; got != st {
				t.Errorf("flipped %s to %s", st, got)
			}
		}
	})
}
