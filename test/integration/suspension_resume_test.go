//go:build integration

package integration

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// scriptedClouds records the walks and runs a step inside each.
type scriptedClouds struct {
	suspended, resumed []string
	during             func(ctx context.Context, bp string)
}

func (c *scriptedClouds) SuspendBillingProfileClouds(ctx context.Context, bp string) error {
	c.suspended = append(c.suspended, bp)
	if c.during != nil {
		c.during(ctx, bp)
	}
	return nil
}

func (c *scriptedClouds) ResumeBillingProfileClouds(ctx context.Context, bp string) error {
	c.resumed = append(c.resumed, bp)
	if c.during != nil {
		c.during(ctx, bp)
	}
	return nil
}

func (c *scriptedClouds) FinishResumes(context.Context) error { return nil }

// A profile resumed while an earlier one is walked is not walked: its customer has just paid.
func TestFinishSuspensionsRereadsEachProfile(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	for _, id := range []string{"bp-a", "bp-b"} {
		mustInsert(t, db, "billingProfile", pgdoc.M{"_id": id, "organizationId": "o", "status": billing.StatusSuspended})
	}
	c := &scriptedClouds{}
	c.during = func(ctx context.Context, bp string) {
		other := "bp-a"
		if bp == "bp-a" {
			other = "bp-b"
		}
		if _, err := db.C("billingProfile").SetByID(ctx, other, pgdoc.M{"status": billing.StatusActive}, nil); err != nil {
			t.Error(err)
		}
	}
	job := billing.NewSuspensionJob(repo, nil)
	job.SetCloudSuspender(c)
	if _, err := job.ExecuteDunning(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.suspended) != 1 {
		t.Fatalf("walked %v, the second was resumed during the first", c.suspended)
	}
}

// The resume leg keeps going when the request goes away, on both paths a request reaches it.
func TestResumeWalkOutlivesTheRequest(t *testing.T) {
	db := freshPG(t)
	repo := billing.NewRepo(db)
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "bp", "organizationId": "o", "status": billing.StatusSuspended})
	rctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var after error
	c := &scriptedClouds{during: func(ctx context.Context, _ string) {
		cancel()
		after = ctx.Err()
	}}
	act := billing.NewActivationService(repo, nil, slog.Default())
	act.SetClouds(c)
	bp, err := repo.FindByID(context.Background(), "bp")
	if err != nil {
		t.Fatal(err)
	}
	if err := act.Unsuspend(rctx, bp, billing.SourceAdmin); err != nil {
		t.Fatal(err)
	}
	if len(c.resumed) != 1 || after != nil {
		t.Fatalf("walks %v, context in the walk after the cancel: %v", c.resumed, after)
	}
}

// A provider removed from a project with nothing billable left does not stop a move. One that is there and cannot answer does: not
// knowing is not "nothing there". What a removed provider left in rows still counts.
func TestMoveWithABadProvider(t *testing.T) {
	f := newSuspendedFixture(t)
	f.h.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(es.PVEConfig(), es.ID, es.PVEPoolPrefix())
	})
	mustInsert(t, f.db, "externalService", pgdoc.M{"_id": "dead-pve", "name": "dead", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "proxmox", "apiUrl": "http://127.0.0.1:1", "tokenId": "t@pam!x", "region": "r1"}})
	bound := func(id string) []any {
		return []any{pgdoc.M{"serviceId": id, "provider": "proxmox", "region": "r1"}}
	}

	t.Run("removed, nothing billable: moved and held", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled, "services": bound("gone-pve")})
		if code, body := f.do(http.MethodPost, "/project/p1/billing/bp-new-o1", ""); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.BillingProfileID != "bp-new-o1" || p.Status != project.StatusDisabled {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("removed, rows left: refused", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled, "services": bound("gone-pve")})
		mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "r1", "projectId": "p1", "serviceId": "gone-pve", "externalId": "r1", "type": cloud.TypeVolume})
		if code, body := f.do(http.MethodPost, "/project/p1/billing/bp-new-o1", ""); code != http.StatusConflict {
			t.Fatalf("%d %s", code, body)
		}
	})
	t.Run("there but not answering: refused, no 500", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled, "services": bound("dead-pve")})
		code, body := f.do(http.MethodPost, "/project/p1/billing/bp-new-o1", "")
		if code != http.StatusServiceUnavailable || strings.Contains(body, "127.0.0.1") {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.BillingProfileID != "" || p.Status != project.StatusEnabled {
			t.Fatalf("moved without knowing: %+v", p)
		}
	})
	t.Run("not answering, onto a profile that pays: no need to ask", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled, "services": bound("dead-pve")})
		if code, body := f.do(http.MethodPost, "/project/p1/billing/bp-active", ""); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
	})
}

// The operator paths apply the rules the client paths have: a profile that pays for a project through its organization cannot be
// deleted, and a move onto what cannot pay holds or refuses.
func TestOperatorPathsKeepTheBillingRules(t *testing.T) {
	const iss, cid = "test-iss", "test-cid"
	f := newSuspendedFixture(t)
	h := admin.NewHandler(admin.NewRepo(f.db), nil, nil, nil, nil, billing.NewRepo(f.db), nil, nil, nil, nil, "", iss, cid)
	h.SetProjectCloudOps(&admin.ProjectCloudOps{HoldForMove: f.h.HoldForOperatorMove})
	r := chi.NewRouter()
	h.Routes(r)
	do := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	ctx := context.Background()

	t.Run("delete of a profile an org pays through is refused", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled}) // o1 pays through bp-cur, no own field
		code, body := do(http.MethodDelete, "/admin/billing-profile/bp-cur", "")
		if code != http.StatusBadRequest {
			t.Fatalf("%d %s", code, body)
		}
		if bp, err := billing.NewRepo(f.db).FindByID(ctx, "bp-cur"); err != nil || bp == nil {
			t.Fatalf("deleted: %v %v", bp, err)
		}
	})
	t.Run("delete of a profile nothing uses goes through", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		if code, body := do(http.MethodDelete, "/admin/billing-profile/bp-new-o1", ""); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		mustInsert(t, f.db, "billingProfile", pgdoc.M{"_id": "bp-new-o1", "organizationId": "o1", "status": billing.StatusNew})
	})
	t.Run("rename leaves the project alone", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "r1", "projectId": "p1", "serviceId": "dns", "externalId": "r1", "type": cloud.TypeBucket})
		if code, body := do(http.MethodPut, "/admin/project/p1", `{"name":"new","organizationId":"o1"}`); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.Status != project.StatusEnabled || p.Name != "new" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("move onto a NEW profile holding something is refused", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "r1", "projectId": "p1", "serviceId": "dns", "externalId": "r1", "type": cloud.TypeBucket})
		code, body := do(http.MethodPut, "/admin/project/p1", `{"name":"p","organizationId":"o1","billingProfileId":"bp-new-o1"}`)
		if code != http.StatusConflict {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.Status != project.StatusEnabled || p.BillingProfileID != "" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("move onto a NEW profile holding nothing disables, not pinned", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		if code, body := do(http.MethodPut, "/admin/project/p1", `{"name":"p","organizationId":"o1","billingProfileId":"bp-new-o1"}`); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.Status != project.StatusDisabled || p.BillingProfileID != "bp-new-o1" || p.DisabledByOperator {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("move of a DISABLED project holding something onto a NEW profile is refused", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled})
		mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "r1", "projectId": "p1", "serviceId": "dns", "externalId": "r1", "type": cloud.TypeBucket})
		code, body := do(http.MethodPut, "/admin/project/p1", `{"name":"p","organizationId":"o1","billingProfileId":"bp-new-o1"}`)
		if code != http.StatusConflict {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.Status != project.StatusDisabled || p.BillingProfileID != "" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("move of a DISABLED project holding nothing onto a NEW profile goes through", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled})
		if code, body := do(http.MethodPut, "/admin/project/p1", `{"name":"p","organizationId":"o1","billingProfileId":"bp-new-o1"}`); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.Status != project.StatusDisabled || p.BillingProfileID != "bp-new-o1" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("move off a suspended profile is the operator's to make", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled, "billingProfileId": "bp-sus"})
		if code, body := do(http.MethodPut, "/admin/project/p1", `{"name":"p","organizationId":"o1","billingProfileId":"bp-active"}`); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
	})
}
