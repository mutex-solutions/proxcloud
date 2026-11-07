//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const suspendedSub = "owner-sub"

type suspendedFixture struct {
	db     *pgdoc.DB
	repo   *project.Repo
	svc    *project.Service
	h      *project.Handler
	router chi.Router
}

// newSuspendedFixture is the real client handler on Postgres: an owner, org o1 on ACTIVE bp-cur, org o2 on NEW bp-new, and one
// PowerDNS provider, the kind that needs nothing live to provision.
func newSuspendedFixture(t *testing.T) *suspendedFixture {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	repo := project.NewRepo(db)
	orgRepo := org.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	svc := project.NewService(repo, orgRepo, billingRepo, users, nil)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	h := project.NewHandler(svc, project.NewPolicy(org.NewPolicy(orgRepo)), org.NewService(orgRepo, billingRepo, nil), users,
		audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo, cloud.NewRepo(db), esSvc, nil, nil, "r1")
	h.SetCephCreds(cephcred.New(db, enc), cephcred.NewKeyRepo(db, enc))
	router := chi.NewRouter()
	h.Routes(router)
	for coll, doc := range map[string]pgdoc.M{
		"users":           {"sub": suspendedSub, "email": "o@example.org"},
		"externalService": {"_id": "dns", "name": "dns", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": pgdoc.M{"provider": "powerdns", "apiUrl": "http://127.0.0.1:1", "region": "r1"}},
	} {
		mustInsert(t, db, coll, doc)
	}
	for id, bp := range map[string]string{"o1": "bp-cur", "o2": "bp-new", "o3": "bp-sus"} {
		mustInsert(t, db, "organization", pgdoc.M{"_id": id, "name": id, "billingProfileId": bp})
		mustInsert(t, db, "organization_members", pgdoc.M{"organizationId": id, "sub": suspendedSub, "roles": []string{"OWNER"}})
	}
	for id, c := range map[string][2]string{
		"bp-cur": {"o1", billing.StatusActive}, "bp-active": {"o1", billing.StatusActive}, "bp-new-o1": {"o1", billing.StatusNew},
		"bp-new": {"o2", billing.StatusNew}, "bp-sus": {"o3", billing.StatusSuspended},
	} {
		mustInsert(t, db, "billingProfile", pgdoc.M{"_id": id, "organizationId": c[0], "status": c[1]})
	}
	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{"_id": "default", "baseCurrency": "USD"}); err != nil {
		t.Fatal(err)
	}
	return &suspendedFixture{db: db, repo: repo, svc: svc, h: h, router: router}
}

func (f *suspendedFixture) seed(t *testing.T, doc pgdoc.M) {
	t.Helper()
	ctx := context.Background()
	_, _ = f.db.C("project").DeleteMany(ctx, pgdoc.M{})
	_, _ = f.db.C("cloudResource").DeleteMany(ctx, pgdoc.M{})
	base := pgdoc.M{"_id": "p1", "name": "p", "organizationId": "o1", "memberships": []pgdoc.M{{"sub": suspendedSub, "role": project.RoleOwner}},
		"services": []any{pgdoc.M{"serviceId": "dns", "provider": "powerdns", "region": "r1"}}}
	for k, v := range doc {
		base[k] = v
	}
	mustInsert(t, f.db, "project", base)
}

func (f *suspendedFixture) do(method, path, body string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: suspendedSub}))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func (f *suspendedFixture) stored(t *testing.T) *project.Project {
	t.Helper()
	p, err := f.repo.FindByID(context.Background(), "p1")
	if err != nil || p == nil {
		t.Fatalf("read back: %v %v", p, err)
	}
	return p
}

// TestDisabledProjectCannotAddUsage: a DISABLED project refuses what adds usage with a message that says why, and keeps reads,
// stops and deletes. Metering covers only ENABLED projects, so anything it started would run free.
func TestDisabledProjectCannotAddUsage(t *testing.T) {
	f := newSuspendedFixture(t)
	f.seed(t, pgdoc.M{"status": project.StatusDisabled})
	mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "z1", "projectId": "p1", "serviceId": "dns", "externalId": "z1", "type": cloud.TypeDNSZone})
	mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "b1", "projectId": "p1", "serviceId": "dns", "externalId": "b1", "type": cloud.TypeBucket})

	refused := []struct{ name, method, path, body string }{
		{"create", http.MethodPost, "/project/p1/cloud", `{"type":"NETWORK","name":"n"}`},
		{"start", http.MethodPost, "/project/p1/cloud/z1/action", `{"action":"START"}`},
		{"start through invoke", http.MethodPost, "/project/p1/cloud/z1/action", `{"action":"INVOKEACTION","data":{"action":"START"}}`},
		{"console", http.MethodPost, "/project/p1/cloud/z1/action", `{"action":"REMOTECONTROL"}`},
		{"a new zone record", http.MethodPost, "/project/p1/cloud/z1/action", `{"action":"SET_RECORD"}`},
		{"bucket upload", http.MethodPost, "/project/p1/cloud/b1/upload-bucket-file?objectName=x", "x"},
		{"new S3 key", http.MethodPost, "/project/p1/s3-keys", `{"name":"k"}`},
		{"rotate S3 credentials", http.MethodPost, "/project/p1/s3-credentials/rotate", ""},
		{"rotate S3 key", http.MethodPost, "/project/p1/s3-keys/k1/rotate", ""},
	}
	for _, c := range refused {
		t.Run("refused: "+c.name, func(t *testing.T) {
			code, body := f.do(c.method, c.path, c.body)
			if code != http.StatusConflict || !strings.Contains(body, "suspended") {
				t.Fatalf("%d %s", code, body)
			}
		})
	}
	// These get past the gate: whatever answers next (a provider that is not there, an unknown action), it is not the gate.
	open := []struct{ name, method, path, body string }{
		{"read a resource", http.MethodGet, "/project/p1/cloud/z1", ""},
		{"read action", http.MethodPost, "/project/p1/cloud/z1/action", `{"action":"LIST_RECORDS"}`},
		{"stop", http.MethodPost, "/project/p1/cloud/z1/action", `{"action":"STOP"}`},
		{"delete", http.MethodDelete, "/project/p1/cloud/z1", ""},
		{"delete S3 key", http.MethodDelete, "/project/p1/s3-keys/k1", ""},
		{"list S3 keys", http.MethodGet, "/project/p1/s3-keys", ""},
	}
	for _, c := range open {
		t.Run("open: "+c.name, func(t *testing.T) {
			if _, body := f.do(c.method, c.path, c.body); strings.Contains(body, "suspended") {
				t.Fatal(body)
			}
		})
	}
	t.Run("an enabled project is not gated", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "z1", "projectId": "p1", "serviceId": "dns", "externalId": "z1", "type": cloud.TypeDNSZone})
		if _, body := f.do(http.MethodPost, "/project/p1/cloud/z1/action", `{"action":"START"}`); strings.Contains(body, "suspended") {
			t.Fatal(body)
		}
	})

}

// TestMoveToAProfileThatCannotPay: moving onto a NEW profile disables the project, and is refused while something would run
// unbilled. An org move passes the same checks, so it is no way out of a suspended profile.
func TestMoveToAProfileThatCannotPay(t *testing.T) {
	f := newSuspendedFixture(t)
	org := func(id string) string {
		b, _ := json.Marshal(map[string]string{"organizationId": id})
		return string(b)
	}

	t.Run("nothing running: moved and disabled", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		if code, body := f.do(http.MethodPost, "/project/p1/billing/bp-new-o1", ""); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.Status != project.StatusDisabled || p.BillingProfileID != "bp-new-o1" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("something running: refused, nothing changed", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "c1", "projectId": "p1", "serviceId": "dns", "externalId": "c1", "type": cloud.TypeKubernetesCluster})
		code, body := f.do(http.MethodPost, "/project/p1/billing/bp-new-o1", "")
		if code != http.StatusConflict || !strings.Contains(body, "unbilled") {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.Status != project.StatusEnabled || p.BillingProfileID != "" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("org move onto a NEW profile disables and takes the org's billing", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled, "billingProfileId": "bp-active"})
		if code, body := f.do(http.MethodPut, "/project/p1/organization", org("o2")); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.Status != project.StatusDisabled || p.OrganizationID != "o2" || p.BillingProfileID != "" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("org move out of a suspended profile refused", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled, "organizationId": "o3"})
		if code, body := f.do(http.MethodPut, "/project/p1/organization", org("o1")); code != 400 {
			t.Fatalf("%d %s", code, body)
		}
		if p := f.stored(t); p.OrganizationID != "o3" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("org move onto a suspended profile refused", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		if code, body := f.do(http.MethodPut, "/project/p1/organization", org("o3")); code != 400 {
			t.Fatalf("%d %s", code, body)
		}
	})
	t.Run("org move while being deleted refused", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDeleteInProgress})
		if code, body := f.do(http.MethodPut, "/project/p1/organization", org("o1")); code != 400 {
			t.Fatalf("%d %s", code, body)
		}
	})
}

// TestMoveOntoWhatCannotPayHoldsOrRefuses: an organization with no profile, or a deleted one, follows the NEW-profile rule; so does
// anything billable a project holds, storage included; and with billing unconfigured there is nothing to hold.
func TestMoveOntoWhatCannotPayHoldsOrRefuses(t *testing.T) {
	f := newSuspendedFixture(t)
	mustInsert(t, f.db, "organization", pgdoc.M{"_id": "o4", "name": "o4"})
	mustInsert(t, f.db, "organization_members", pgdoc.M{"organizationId": "o4", "sub": suspendedSub, "roles": []string{"OWNER"}})
	mustInsert(t, f.db, "organization", pgdoc.M{"_id": "o5", "name": "o5", "billingProfileId": "gone"})
	mustInsert(t, f.db, "organization_members", pgdoc.M{"organizationId": "o5", "sub": suspendedSub, "roles": []string{"OWNER"}})
	hold := func(t *testing.T, rowType string) {
		t.Helper()
		mustInsert(t, f.db, "cloudResource", pgdoc.M{"_id": "r1", "projectId": "p1", "serviceId": "dns", "externalId": "r1", "type": rowType})
	}
	for _, target := range []string{"o4", "o5"} {
		t.Run(target+": holds a cluster, refused", func(t *testing.T) {
			f.seed(t, pgdoc.M{"status": project.StatusEnabled})
			hold(t, cloud.TypeKubernetesCluster)
			code, body := f.do(http.MethodPut, "/project/p1/organization", `{"organizationId":"`+target+`"}`)
			if code != http.StatusConflict || !strings.Contains(body, "no billing profile") {
				t.Fatalf("%d %s", code, body)
			}
			if p := f.stored(t); p.Status != project.StatusEnabled || p.OrganizationID != "o1" {
				t.Fatalf("%+v", p)
			}
		})
		t.Run(target+": holds nothing, moved and disabled", func(t *testing.T) {
			f.seed(t, pgdoc.M{"status": project.StatusEnabled})
			if code, body := f.do(http.MethodPut, "/project/p1/organization", `{"organizationId":"`+target+`"}`); code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			if p := f.stored(t); p.Status != project.StatusDisabled || p.OrganizationID != target {
				t.Fatalf("%+v", p)
			}
		})
	}
	for _, rowType := range []string{cloud.TypeBucket, cloud.TypeVolume, cloud.TypeFloatingIP, cloud.TypeShare, cloud.TypeImage, cloud.TypeServer} {
		t.Run("refused while it holds "+rowType, func(t *testing.T) {
			f.seed(t, pgdoc.M{"status": project.StatusEnabled})
			hold(t, rowType)
			code, body := f.do(http.MethodPost, "/project/p1/billing/bp-new-o1", "")
			if code != http.StatusConflict || !strings.Contains(body, "unbilled") {
				t.Fatalf("%d %s", code, body)
			}
			if p := f.stored(t); p.Status != project.StatusEnabled || p.BillingProfileID != "" {
				t.Fatalf("%+v", p)
			}
		})
	}
	t.Run("a DISABLED project cannot park what it holds either", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled, "billingProfileId": "bp-cur"})
		hold(t, cloud.TypeBucket)
		if code, body := f.do(http.MethodPost, "/project/p1/billing/bp-new-o1", ""); code != http.StatusConflict {
			t.Fatalf("%d %s", code, body)
		}
	})
	t.Run("what is not billable does not refuse", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		hold(t, cloud.TypeNetwork)
		if code, body := f.do(http.MethodPost, "/project/p1/billing/bp-new-o1", ""); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
	})

	if _, err := f.db.C("billingConfiguration").DeleteMany(context.Background(), pgdoc.M{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"o2", "a NEW profile"}, {"o4", "no profile"}} {
		t.Run("billing unconfigured: a move to "+c[1]+" holds nothing", func(t *testing.T) {
			f.seed(t, pgdoc.M{"status": project.StatusEnabled})
			hold(t, cloud.TypeKubernetesCluster)
			if code, body := f.do(http.MethodPut, "/project/p1/organization", `{"organizationId":"`+c[0]+`"}`); code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			if p := f.stored(t); p.Status != project.StatusEnabled || p.OrganizationID != c[0] {
				t.Fatalf("%+v", p)
			}
		})
	}
}

// TestDeletingAProfileThatMatters: deleting the suspended profile, or the one that pays, would free the project from it.
func TestDeletingAProfileThatMatters(t *testing.T) {
	f := newSuspendedFixture(t)
	ctx := context.Background()
	orgRepo := org.NewRepo(f.db)
	billingRepo := billing.NewRepo(f.db)
	org.NewBillingHandler(org.NewService(orgRepo, billingRepo, nil), org.NewPolicy(orgRepo), billingRepo, pricing.NewRepo(f.db),
		nil, nil, nil, nil, "", user.NewRepo(f.db), nil).Routes(f.router)
	del := func(t *testing.T, id string, want int, part string) {
		t.Helper()
		code, body := f.do(http.MethodDelete, "/billing-profile/"+id, "")
		if code != want || !strings.Contains(body, part) {
			t.Fatalf("delete %s: %d %s", id, code, body)
		}
	}
	t.Run("suspended, then the move it would free", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled, "organizationId": "o3"})
		del(t, "bp-sus", http.StatusConflict, "suspended")
		if code, body := f.do(http.MethodPut, "/project/p1/organization", `{"organizationId":"o1"}`); code != http.StatusBadRequest {
			t.Fatalf("the suspension must still hold the project: %d %s", code, body)
		}
	})
	t.Run("pays through the organization", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled, "name": "web"})
		del(t, "bp-cur", http.StatusConflict, "project web")
	})
	t.Run("pays as the project's own", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled, "billingProfileId": "bp-new"})
		del(t, "bp-new", http.StatusConflict, "still pays")
	})
	t.Run("negative balance", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		mustInsert(t, f.db, "bill", pgdoc.M{"billingProfileId": "bp-new", "status": "SENT", "invoiceCurrency": "USD",
			"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "5")}}})
		del(t, "bp-new", http.StatusConflict, "negative balance")
		_, _ = f.db.C("bill").DeleteMany(ctx, pgdoc.M{})
	})
	t.Run("a deleted project does not hold it, and a free one goes", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDeleted, "organizationId": "o2"})
		del(t, "bp-new", http.StatusAccepted, "")
		if bp, err := billingRepo.FindByID(ctx, "bp-new"); err != nil || bp != nil {
			t.Fatalf("%v %v", bp, err)
		}
	})
}

// A walk that behaves like the real one: the status moves first, then it takes as long as the VMs take.
type walk struct {
	f             *suspendedFixture
	t             *testing.T
	err           error
	during        func()
	suspends, res int
}

func (w *walk) SuspendBillingProfileClouds(ctx context.Context, bp string) error {
	w.suspends++
	if w.err != nil {
		return w.err
	}
	if _, err := w.f.repo.ApplyBillingSuspension(ctx, "p1", bp, true); err != nil {
		return err
	}
	if w.during != nil {
		w.during()
	}
	return nil
}

func (w *walk) ResumeBillingProfileClouds(ctx context.Context, bp string) error {
	w.res++
	_, err := w.f.repo.ApplyBillingSuspension(ctx, "p1", bp, false)
	return err
}

// TestOpenDuringASuspendWalk: the profile reads SUSPENDED before the walk starts, so a member opening the project it has just
// disabled does not lift it, on either path.
func TestOpenDuringASuspendWalk(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, f *suspendedFixture) {
		t.Helper()
		bp, _ := billing.NewRepo(f.db).FindByID(ctx, "bp-cur")
		if p := f.stored(t); bp.Status != billing.StatusSuspended || p.Status != project.StatusDisabled {
			t.Fatalf("suspension lost: profile %s, project %s", bp.Status, p.Status)
		}
	}
	open := func(f *suspendedFixture) func() {
		return func() { f.do(http.MethodPost, "/project/p1/init", "") }
	}
	t.Run("operator suspend", func(t *testing.T) {
		f := newSuspendedFixture(t)
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		repo := billing.NewRepo(f.db)
		svc := billing.NewActivationService(repo, audit.NewService(audit.NewRepo(f.db), slog.Default()), slog.Default())
		svc.SetClouds(&walk{f: f, t: t, during: open(f)})
		bp, _ := repo.FindByID(ctx, "bp-cur")
		if err := svc.Suspend(ctx, bp, billing.SourceAdmin); err != nil {
			t.Fatal(err)
		}
		check(t, f)
	})
	t.Run("dunning", func(t *testing.T) {
		f := newSuspendedFixture(t)
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		owes(t, f)
		job := billing.NewSuspensionJob(billing.NewRepo(f.db), nil)
		job.SetCloudSuspender(&walk{f: f, t: t, during: open(f)})
		if n, err := job.ExecuteDunning(ctx); err != nil || n != 1 {
			t.Fatalf("%d %v", n, err)
		}
		check(t, f)
	})
}

// owes turns on dunning and puts bp-cur past its limit.
func owes(t *testing.T, f *suspendedFixture) {
	t.Helper()
	_, _ = f.db.C("billingConfiguration").DeleteMany(context.Background(), pgdoc.M{})
	mustInsert(t, f.db, "billingConfiguration", pgdoc.M{"_id": "default", "baseCurrency": "USD", "suspensionConfiguration": pgdoc.M{
		"enabled": true, "type": "BALANCE", "suspendedAt": pgdoc.M{"balance": decimalOf(t, "-100"), "days": 0}, "notifications": []any{},
	}})
	mustInsert(t, f.db, "bill", pgdoc.M{"billingProfileId": "bp-cur", "status": "SENT", "invoiceCurrency": "USD",
		"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "150")}}})
}

// TestAFailedWalkIsCaughtByTheNextPass: the chosen order. A walk failing leaves the profile SUSPENDED, never ACTIVE with projects
// down, and the job walks every SUSPENDED profile again, whichever way it got there and with dunning off.
func TestAFailedWalkIsCaughtByTheNextPass(t *testing.T) {
	ctx := context.Background()
	f := newSuspendedFixture(t)
	f.seed(t, pgdoc.M{"status": project.StatusEnabled})
	repo := billing.NewRepo(f.db)
	w := &walk{f: f, t: t, err: errors.New("PVE gone")}
	svc := billing.NewActivationService(repo, nil, slog.Default())
	svc.SetClouds(w)
	bp, _ := repo.FindByID(ctx, "bp-cur")
	if err := svc.Suspend(ctx, bp, billing.SourceAdmin); err != nil {
		t.Fatal(err)
	}
	stored, _ := repo.FindByID(ctx, "bp-cur")
	if p := f.stored(t); stored.Status != billing.StatusSuspended || p.Status != project.StatusEnabled {
		t.Fatalf("profile %s, project %s", stored.Status, p.Status)
	}
	job := billing.NewSuspensionJob(repo, nil)
	job.SetCloudSuspender(w)
	if _, err := job.ExecuteDunning(ctx); err != nil { // the fixture's config has no dunning at all
		t.Fatal(err)
	}
	if p := f.stored(t); p.Status != project.StatusEnabled {
		t.Fatalf("still failing: %s", p.Status)
	}
	w.err = nil
	if _, err := job.ExecuteDunning(ctx); err != nil {
		t.Fatal(err)
	}
	if p := f.stored(t); p.Status != project.StatusDisabled {
		t.Fatalf("the pass must finish it: %s", p.Status)
	}

	t.Run("a resume landing during the walk stands", func(t *testing.T) {
		w.during = func() {
			fresh, _ := repo.FindByID(ctx, "bp-cur")
			if err := svc.Unsuspend(ctx, fresh, billing.SourceAdmin); err != nil {
				t.Error(err)
			}
			// What the resume walked past is taken down by the walk still running.
			if _, err := f.repo.ApplyBillingSuspension(ctx, "p1", "bp-cur", true); err != nil {
				t.Error(err)
			}
		}
		if _, err := job.ExecuteDunning(ctx); err != nil {
			t.Fatal(err)
		}
		fresh, _ := repo.FindByID(ctx, "bp-cur")
		if p := f.stored(t); fresh.Status != billing.StatusActive || p.Status != project.StatusEnabled {
			t.Fatalf("profile %s, project %s", fresh.Status, p.Status)
		}
	})
	t.Run("a suspend whose profile save failed carries on", func(t *testing.T) {
		f := newSuspendedFixture(t)
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		owes(t, f)
		repo := billing.NewRepo(f.db)
		if err := repo.SaveSuspensionProcess(ctx, &billing.SuspensionProcess{Status: billing.SuspensionSuspended, BillingProfileID: "bp-cur"}); err != nil {
			t.Fatal(err)
		}
		job := billing.NewSuspensionJob(repo, nil)
		job.SetCloudSuspender(&walk{f: f, t: t})
		if n, err := job.ExecuteDunning(ctx); err != nil || n != 1 {
			t.Fatalf("%d %v", n, err)
		}
		if p := f.stored(t); p.Status != project.StatusDisabled {
			t.Fatalf("%s", p.Status)
		}
	})
}

// TestWhatLiftsADisable: opening lifts what nothing holds any more, and only that. An operator's disable and a profile that
// cannot pay keep it down; activation lifts a provisioned project too; the admin API's provision ends an operator's disable.
func TestWhatLiftsADisable(t *testing.T) {
	f := newSuspendedFixture(t)
	ctx := context.Background()
	cases := []struct {
		name string
		doc  pgdoc.M
		want string
	}{
		{"unprovisioned under an ACTIVE profile (admin API project)", pgdoc.M{"status": project.StatusDisabled, "services": []any{}}, project.StatusEnabled},
		{"provisioned under an ACTIVE profile, moved back from NEW", pgdoc.M{"status": project.StatusDisabled, "billingProfileId": "bp-active"}, project.StatusEnabled},
		{"under a NEW profile", pgdoc.M{"status": project.StatusDisabled, "billingProfileId": "bp-new-o1", "services": []any{}}, project.StatusDisabled},
		{"under a SUSPENDED profile", pgdoc.M{"status": project.StatusDisabled, "organizationId": "o3", "services": []any{}}, project.StatusDisabled},
		{"disabled by an operator", pgdoc.M{"status": project.StatusDisabled, "disabledByOperator": true, "services": []any{}}, project.StatusDisabled},
		{"being deleted", pgdoc.M{"status": project.StatusDeleteInProgress, "services": []any{}}, project.StatusDeleteInProgress},
	}
	for _, c := range cases {
		t.Run("open: "+c.name, func(t *testing.T) {
			f.seed(t, c.doc)
			if code, body := f.do(http.MethodPost, "/project/p1/init", ""); code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			if got := f.stored(t).Status; got != c.want {
				t.Fatalf("%s, want %s", got, c.want)
			}
		})
	}
	t.Run("activation lifts a provisioned project", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled})
		if err := f.h.EnableAndBootstrap(ctx, f.stored(t)); err != nil {
			t.Fatal(err)
		}
		if got := f.stored(t).Status; got != project.StatusEnabled {
			t.Fatal(got)
		}
	})
	t.Run("activation never lifts an operator's disable", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled, "disabledByOperator": true})
		if err := f.h.EnableAndBootstrap(ctx, f.stored(t)); err == nil {
			t.Fatal("want a refusal")
		}
		if got := f.stored(t).Status; got != project.StatusDisabled {
			t.Fatal(got)
		}
	})
	t.Run("provision ends an operator's disable of an unprovisioned project", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled, "disabledByOperator": true, "services": []any{}})
		if err := f.h.ProvisionByOperator(ctx, f.stored(t)); err != nil {
			t.Fatal(err)
		}
		if p := f.stored(t); p.Status != project.StatusEnabled || p.DisabledByOperator || !p.HasService("dns") {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("provision leaves a provisioned DISABLED project", func(t *testing.T) {
		f.seed(t, pgdoc.M{"status": project.StatusDisabled, "disabledByOperator": true})
		if err := f.h.ProvisionByOperator(ctx, f.stored(t)); err != nil {
			t.Fatal(err)
		}
		if p := f.stored(t); p.Status != project.StatusDisabled || !p.DisabledByOperator {
			t.Fatalf("%+v", p)
		}
	})
}

// TestBootstrapKeepsADeletionMeanwhile is the reported race: a member's DeleteNow lands while bootstrap waits on a provider. The
// bootstrap's saves hold the read from before it, and must not put the status back, or the project drops out of the sweep.
func TestBootstrapKeepsADeletionMeanwhile(t *testing.T) {
	f := newSuspendedFixture(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	rgw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		_, _ = w.Write([]byte(`{"user_id":"u","keys":[{"user":"u","access_key":"a","secret_key":"s"}]}`))
	}))
	defer rgw.Close()
	enc := textcrypt.New("test-key")
	mustInsert(t, f.db, "externalService", pgdoc.M{
		"_id": "s3", "name": "s3", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "ceph-s3", "s3Endpoint": rgw.URL, "adminApiUrl": rgw.URL + "/admin", "region": "r1"},
		"secret": pgdoc.M{"adminAccessKey": enc.Encrypt("AK"), "adminSecretKey": enc.Encrypt("SK")},
	})
	for _, from := range []string{project.StatusEnabled, project.StatusDisabled} {
		t.Run("from "+from, func(t *testing.T) {
			entered, release = make(chan struct{}), make(chan struct{})
			once = sync.Once{}
			f.seed(t, pgdoc.M{"status": from, "services": []any{}})
			done := make(chan error, 1)
			go func() { done <- f.h.EnableAndBootstrap(ctx, f.stored(t)) }()
			<-entered
			if _, err := f.svc.DeleteNow(ctx, "p1"); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Logf("bootstrap: %v", err)
			}
			p := f.stored(t)
			if p.Status != project.StatusDeleteInProgress || p.ScheduledForDeletionAt == nil {
				t.Fatalf("status %s, schedule %v", p.Status, p.ScheduledForDeletionAt)
			}
		})
	}
}

// TestSuspensionSurvivesAStatusChangeMeanwhile: a suspension racing a member's schedule must land either way, so a cancel after
// it restores DISABLED. A write decided on a stale status loses the conditional write, and has to retry.
func TestSuspensionSurvivesAStatusChangeMeanwhile(t *testing.T) {
	f := newSuspendedFixture(t)
	ctx := context.Background()
	for i := range 40 {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		member := f.stored(t)
		var wg sync.WaitGroup
		wg.Add(2)
		var applyErr error
		go func() { defer wg.Done(); _, applyErr = f.repo.ApplyBillingSuspension(ctx, "p1", "bp-cur", true) }()
		go func() { defer wg.Done(); _, _ = f.svc.ScheduleDeletion(ctx, member, false) }()
		wg.Wait()
		if applyErr != nil {
			t.Fatalf("run %d: %v", i, applyErr)
		}
		if p := f.stored(t); p.Status == project.StatusScheduledForDeletion {
			if _, err := f.svc.CancelDeletion(ctx, "p1"); err != nil {
				t.Fatalf("run %d: cancel: %v", i, err)
			}
		}
		if got := f.stored(t).Status; got != project.StatusDisabled {
			t.Fatalf("run %d: the suspension was lost: %s", i, got)
		}
	}
}

// TestProfileOnlyGovernsWhatItPaysFor: a project on its own profile is not picked up through the org's, so the org's resume
// cannot lift it. A project with no profile of its own is, with the field absent or blank.
func TestProfileOnlyGovernsWhatItPaysFor(t *testing.T) {
	f := newSuspendedFixture(t)
	ctx := context.Background()
	_, _ = f.db.C("project").DeleteMany(ctx, pgdoc.M{})
	mustInsert(t, f.db, "project", pgdoc.M{"_id": "own", "name": "own", "status": project.StatusDisabled, "organizationId": "o1", "billingProfileId": "bp-sus"})
	mustInsert(t, f.db, "project", pgdoc.M{"_id": "none", "name": "none", "status": project.StatusDisabled, "organizationId": "o1"})
	mustInsert(t, f.db, "project", pgdoc.M{"_id": "blank", "name": "blank", "status": project.StatusDisabled, "organizationId": "o1", "billingProfileId": ""})
	mustInsert(t, f.db, "project", pgdoc.M{"_id": "direct", "name": "direct", "status": project.StatusDisabled, "organizationId": "o3", "billingProfileId": "bp-cur"})
	list, err := f.repo.AllByBillingProfile(ctx, "bp-cur", []string{"o1"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for i := range list {
		got[list[i].ID] = true
	}
	if len(got) != 3 || !got["none"] || !got["blank"] || !got["direct"] {
		t.Fatalf("%v", got)
	}
	// Even handed its id, the org's resume leaves it.
	p, err := f.repo.ApplyBillingSuspension(ctx, "own", "bp-cur", false)
	if err != nil || p.Status != project.StatusDisabled {
		t.Fatalf("%+v %v", p, err)
	}
}

// TestAdminWritesLeaveTheStatusAlone: an operator's project update and quota write only their fields, and disabling a project
// only billing holds pins it.
func TestAdminWritesLeaveTheStatusAlone(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := project.NewRepo(db)
	const iss, cid = "test-iss", "test-cid"
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, nil, "", iss, cid)
	h.SetProjectCloudOps(&admin.ProjectCloudOps{PauseServers: func(context.Context, string, bool) error { return nil }})
	router := chi.NewRouter()
	h.Routes(router)
	do := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}
	mustInsert(t, db, "project", pgdoc.M{"_id": "p1", "name": "p", "status": project.StatusDisabled, "organizationId": "o", "billingProfileId": "b", "services": []any{}})

	t.Run("update writes its three fields and clears what is absent", func(t *testing.T) {
		if code := do(http.MethodPut, "/admin/project/p1", `{"name":"q","organizationId":"o"}`); code != 200 {
			t.Fatal(code)
		}
		p, _ := repo.FindByID(ctx, "p1")
		if p.Name != "q" || p.BillingProfileID != "" || p.Status != project.StatusDisabled {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("pin a billing disable, once", func(t *testing.T) {
		if code := do(http.MethodPost, "/admin/project/p1/DISABLED", ""); code != 200 {
			t.Fatal(code)
		}
		p, _ := repo.FindByID(ctx, "p1")
		if !p.DisabledByOperator || p.Status != project.StatusDisabled {
			t.Fatalf("%+v", p)
		}
		if p.ApplyBillingSuspension("b", false) {
			t.Fatal("a resume must leave a pinned project")
		}
		if code := do(http.MethodPost, "/admin/project/p1/DISABLED", ""); code != 400 {
			t.Fatal(code)
		}
	})
	t.Run("quota keeps a status changed since the read", func(t *testing.T) {
		if code := do(http.MethodPut, "/admin/project/p1/quota", `{"gpu":{"*":1}}`); code != 200 {
			t.Fatal(code)
		}
		p, _ := repo.FindByID(ctx, "p1")
		if len(p.Quota) == 0 || !p.DisabledByOperator {
			t.Fatalf("%+v", p)
		}
		if code := do(http.MethodPut, "/admin/project/p1/quota", `{}`); code != 200 {
			t.Fatal(code)
		}
		p, _ = repo.FindByID(ctx, "p1")
		if len(p.Quota) != 0 {
			t.Fatalf("%+v", p)
		}
	})
}

// TestConditionalWriteHasOneWinner: writers racing on the same expected status get one winner. Every status rule rests on this.
func TestConditionalWriteHasOneWinner(t *testing.T) {
	f := newSuspendedFixture(t)
	ctx := context.Background()
	for i := range 30 {
		f.seed(t, pgdoc.M{"status": project.StatusEnabled})
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			p := f.stored(t)
			p.Status = project.StatusDisabled
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ok, err := f.repo.SaveIfStatus(ctx, p, project.StatusEnabled); err == nil && ok {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("run %d: %d winners", i, wins.Load())
		}
	}
}
