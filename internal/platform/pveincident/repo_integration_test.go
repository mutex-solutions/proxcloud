//go:build integration

package pveincident

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type fixture struct {
	db    *pgdoc.DB
	repo  *Repo
	audit *audit.Service
	tick  atomic.Int64
	base  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("incident_fixture"), tcpostgres.WithUsername("fixture"), tcpostgres.WithPassword("fixture"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgdoc.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(ctx) })
	f := &fixture{db: db, base: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	f.audit = audit.NewService(audit.NewRepo(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.repo = f.restart()
	if err = f.repo.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	f.seedProvider(t, "provider1")
	return f
}
func (f *fixture) seedProvider(t *testing.T, provider string) {
	t.Helper()
	es := externalservice.ExternalService{ID: provider, Type: externalservice.TypeCloud, Status: "PUBLIC", Config: map[string]any{"provider": "proxmox", "region": "region1", "tokenId": "operator@pve!portal"}, Secret: map[string]any{"tokenSecret": textcrypt.New("fixture-key").Encrypt("credential-A-canary")}}
	if err := f.db.C("externalService").Upsert(context.Background(), provider, es); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) generation(t *testing.T, provider string) string {
	t.Helper()
	var es externalservice.ExternalService
	if found, err := f.db.C("externalService").Get(context.Background(), provider, &es); err != nil || !found {
		t.Fatal("fixture provider", err)
	}
	return externalservice.RawPVEObservationGeneration(&es)
}
func (f *fixture) restart() *Repo {
	r := New(f.db, f.audit)
	// Only this disposable DB fixture overrides the transaction clock. No sleep,
	// production backdating, app clock or exported clock/config bypass is needed.
	r.clock = func(ctx context.Context) (time.Time, error) {
		if _, err := f.db.Tx(ctx); err != nil {
			return time.Time{}, err
		}
		return f.base.Add(time.Duration(f.tick.Load()) * time.Second), nil
	}
	return r
}
func (f *fixture) snapshot(t *testing.T, status NodeStatus) SnapshotResult {
	t.Helper()
	c, err := f.repo.BeginCapture(context.Background(), "provider1", f.generation(t, "provider1"), "region1")
	if err != nil {
		t.Fatal(err)
	}
	x := trusted()
	x.Capture = c
	x.Nodes = []NodeFact{{"node1", status}}
	x.Links = []Link{{"serverA", "projectA", "instanceA", "node1"}, {"serverB", "projectB", "instanceB", "node1"}, {"serverC", "projectA", "instanceC", "node1"}}
	return x
}
func observe(t *testing.T, r *Repo, x SnapshotResult) ObserveResult {
	t.Helper()
	out, err := r.Observe(context.Background(), x)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func incident(t *testing.T, f *fixture, kind string) Incident {
	t.Helper()
	page, err := f.repo.List(context.Background(), "provider1", Filter{Kind: kind})
	if err != nil || len(page.Data) != 1 {
		t.Fatalf("incident page %+v %v", page, err)
	}
	return page.Data[0]
}

func TestIncidentPGReplicasRestartThrottleRecoveryAndSelectors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	other := f.restart()
	var previous SnapshotResult
	for i := 0; i < 3; i++ {
		f.tick.Store(int64(i * 60))
		x := f.snapshot(t, Offline)
		if i > 0 {
			if _, err := f.repo.Observe(ctx, previous); err != ErrInvalid {
				t.Fatal("stale epoch accepted", err)
			}
		}
		var wg sync.WaitGroup
		var accepted, events atomic.Int64
		for j := 0; j < 8; j++ {
			wg.Go(func() {
				r := f.repo
				if j%2 == 1 {
					r = other
				}
				out, err := r.Observe(ctx, x)
				if err != nil {
					t.Error(err)
					return
				}
				if out.Accepted {
					accepted.Add(1)
				}
				events.Add(int64(out.Events))
			})
		}
		wg.Wait()
		if accepted.Load() != 1 || (events.Load() == 1) != (i == 2) {
			t.Fatal("pod dedup failed", accepted.Load(), events.Load())
		}
		if _, err := other.BeginCapture(ctx, "provider1", x.Capture.Generation, "region1"); err != ErrThrottled {
			t.Fatal("provider throttle bypass", err)
		}
		previous = x
		f.repo = f.restart()
	}
	s := incident(t, f, KindNode)
	if s.State != StateOpen || s.Episode != 1 {
		t.Fatal(s)
	}
	page, err := f.repo.Affected(ctx, "provider1", s.ID, "projectA", "", 1)
	if err != nil || len(page.Data) != 1 || page.Data[0].ServerID != "serverA" || page.Next != "serverA" {
		t.Fatal(page, err)
	}
	page, err = f.repo.Affected(ctx, "provider1", s.ID, "projectA", page.Next, 1)
	if err != nil || len(page.Data) != 1 || page.Data[0].ServerID != "serverC" || page.Next != "" {
		t.Fatal(page, err)
	}
	if _, err = f.repo.Affected(ctx, "foreign", s.ID, "", "", 1); err != ErrNotFound {
		t.Fatal("foreign selector", err)
	}
	f.tick.Store(180)
	x := f.snapshot(t, Online)
	x.Trust.Nodes = Untrusted
	x.Trust.VMs = Untrusted
	x.Links = nil
	observe(t, f.repo, x)
	if s = incident(t, f, KindNode); s.State != StateOpen || s.AffectedComplete {
		t.Fatal("unknown recovered or trusted affected", s)
	}
	page, err = f.repo.Affected(ctx, "provider1", s.ID, "projectA", "", 10)
	if err != nil || len(page.Data) != 2 {
		t.Fatal("last verified IDs lost", page, err)
	}
	for _, tick := range []int64{240, 300, 360} {
		f.tick.Store(tick)
		observe(t, f.repo, f.snapshot(t, Online))
	}
	if s = incident(t, f, KindNode); s.State != StateRecovered || s.RecoveredAt == nil {
		t.Fatal(s)
	}
	var records []Event
	if err = f.db.C("pveIncidentEvent").Find(ctx, pgdoc.M{"incidentId": s.ID}, &records); err != nil || len(records) != 2 {
		t.Fatal("duplicate recovery", records, err)
	}
	n, err := f.audit.Count(ctx, audit.Filter{ResourceType: "PVE_INCIDENT", ResourceID: s.ID})
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	var audits []audit.AuditEvent
	_ = f.db.C("auditEvent").Find(ctx, nil, &audits)
	for _, a := range audits {
		if a.ProjectID != "" || a.OrganizationID != "" || a.ResourceMetadata["observedAt"] == nil {
			t.Fatal("audit isolation/DB observedAt", a)
		}
	}
	f.tick.Store(420)
	x = f.snapshot(t, Offline)
	x.Nodes = nil
	observe(t, f.repo, x)
	if s = incident(t, f, KindNode); s.State != StateRecovered || s.Reason != "subject_unobserved" {
		t.Fatal("absence proved down", s)
	}
	f.tick.Store(480)
	x = f.snapshot(t, Offline)
	x.Nodes = []NodeFact{{"node2", Offline}}
	x.Links = []Link{{"serverA", "projectA", "instance-new", "node2"}}
	x.LinksComplete = false
	observe(t, f.repo, x)
	page, err = f.repo.Affected(ctx, "provider1", s.ID, "projectA", "", 10)
	if err != nil || len(page.Data) != 1 || page.Data[0].ServerID != "serverC" {
		t.Fatal("migration/reuse stale link", page, err)
	}
}

func TestIncidentPGAuditFailureRollsBackAndEpochFreshness(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tick := range []int64{0, 60} {
		f.tick.Store(tick)
		observe(t, f.repo, f.snapshot(t, Offline))
	}
	f.tick.Store(120)
	x := f.snapshot(t, Offline)
	_, err := f.db.Pool.Exec(ctx, `ALTER TABLE "auditEvent" ADD CONSTRAINT fixture_reject_audit CHECK (doc->>'resourceType' <> 'PVE_INCIDENT')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.Observe(ctx, x); err == nil {
		t.Fatal("audit failure swallowed")
	}
	if s := incident(t, f, KindNode); s.State == StateOpen || s.Version != 2 {
		t.Fatal("partial state committed", s)
	}
	var events []Event
	_ = f.db.C("pveIncidentEvent").Find(ctx, nil, &events)
	if len(events) != 0 {
		t.Fatal("partial event committed")
	}
	_, err = f.db.Pool.Exec(ctx, `ALTER TABLE "auditEvent" DROP CONSTRAINT fixture_reject_audit`)
	if err != nil {
		t.Fatal(err)
	}
	if got := observe(t, f.repo, x); !got.Accepted || got.Events != 1 {
		t.Fatal("rollback consumed capture", got)
	}
	f.tick.Store(180)
	x = f.snapshot(t, Online)
	f.tick.Store(301)
	if _, err = f.repo.Observe(ctx, x); err != ErrInvalid {
		t.Fatal("stale capture", err)
	}
	f.tick.Store(360)
	x = f.snapshot(t, Online)
	f.tick.Store(359)
	if _, err = f.repo.Observe(ctx, x); err != ErrInvalid {
		t.Fatal("future capture", err)
	}
	f.tick.Store(420)
	x = f.snapshot(t, Online)
	forged := x
	forged.Capture.StartedAt = forged.Capture.StartedAt.Add(time.Second)
	if _, err = f.repo.Observe(ctx, forged); err != ErrInvalid {
		t.Fatal("forged capture", err)
	}
	x.Trust.Protection = Trust("raw-secret-error")
	if _, err = f.repo.Observe(ctx, x); err != ErrInvalid {
		t.Fatal("raw metadata", err)
	}
	x.Trust.Protection = Verified
	observe(t, f.repo, x)
	if s := incident(t, f, KindNode); s.State != StateOpen {
		t.Fatal("unknown gap allowed early recovery", s)
	}
}

func TestIncidentPGMissingKnownSubjectFreezesVisibilityRecovery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tick := range []int64{0, 60, 120} {
		f.tick.Store(tick)
		x := f.snapshot(t, Offline)
		x.Trust.VMs = Untrusted
		observe(t, f.repo, x)
	}
	if incident(t, f, KindNode).State != StateOpen || incident(t, f, KindVisibility).State != StateOpen {
		t.Fatal("fixtures did not open")
	}
	for _, tick := range []int64{180, 240} {
		f.tick.Store(tick)
		x := f.snapshot(t, Online)
		x.Nodes = nil
		observe(t, f.repo, x)
	}
	if incident(t, f, KindNode).State != StateOpen || incident(t, f, KindVisibility).State != StateOpen {
		t.Fatal("missing known node falsely recovered aggregate visibility")
	}
	for _, tick := range []int64{300, 360} {
		f.tick.Store(tick)
		observe(t, f.repo, f.snapshot(t, Online))
	}
	if incident(t, f, KindNode).State != StateRecovered || incident(t, f, KindVisibility).State != StateRecovered {
		t.Fatal("proper healthy facts did not recover once")
	}
	var events []Event
	_ = f.db.C("pveIncidentEvent").Find(ctx, nil, &events)
	if len(events) != 4 {
		t.Fatal("duplicate/missing transitions", events)
	}
	for _, ev := range events {
		if ev.Action == "OPEN" && ev.AffectedComplete {
			t.Fatal("old transition lost incomplete evidence", ev)
		}
		if ev.Action == "RECOVERED" && !ev.AffectedComplete {
			t.Fatal("fresh complete recovery not recorded", ev)
		}
		if ev.EvidenceQuality == "" {
			t.Fatal("transition evidence quality missing", ev)
		}
	}
}

func TestIncidentPGCredentialRotationSeparatesEvidenceAndRejectsInflight(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tick := range []int64{0, 60} {
		f.tick.Store(tick)
		observe(t, f.repo, f.snapshot(t, Offline))
	}
	oldGeneration := f.generation(t, "provider1")
	f.tick.Store(120)
	outstanding := f.snapshot(t, Offline)
	canary := "credential-B-canary-DO-NOT-PERSIST"
	cipher := textcrypt.New("fixture-key").Encrypt(canary)
	if _, err := f.db.C("externalService").SetByID(ctx, "provider1", pgdoc.M{"secret.tokenSecret": cipher}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Observe(ctx, outstanding); err != ErrInvalid {
		t.Fatal("old capture committed after same-ID credential rotation", err)
	}
	if _, err := f.repo.BeginCapture(ctx, "provider1", oldGeneration, "region1"); err != ErrInvalid {
		t.Fatal("stale owner admitted", err)
	}
	f.tick.Store(180)
	b := f.snapshot(t, Offline)
	if b.Capture.Generation == oldGeneration {
		t.Fatal("rotation did not change opaque generation")
	}
	if got := observe(t, f.repo, b); got.Events != 0 {
		t.Fatal("A2bad+Bthird opened incident", got)
	}
	p, err := f.repo.List(ctx, "provider1", Filter{Kind: KindNode})
	if err != nil || len(p.Data) != 2 {
		t.Fatal(p, err)
	}
	for _, s := range p.Data {
		if s.State == StateOpen {
			t.Fatal("mixed credentials confirmed incident", s)
		}
	}
	f.tick.Store(240)
	pending := f.snapshot(t, Offline)
	if _, err = f.db.C("externalService").SetByID(ctx, "provider1", pgdoc.M{"status": "DISABLED"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = f.repo.Observe(ctx, pending); err != ErrInvalid {
		t.Fatal("disabled provider admitted stale work", err)
	}
	for _, table := range []string{"pveIncidentState", "pveIncidentCapture", "pveIncidentEvent", "auditEvent"} {
		var leak bool
		if err = f.db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM "`+table+`" WHERE doc::text LIKE '%' || $1 || '%' OR doc::text LIKE '%' || $2 || '%')`, canary, cipher).Scan(&leak); err != nil || leak {
			t.Fatal("credential leaked into incident records", table, leak, err)
		}
	}
}

func TestIncidentPGRealClockAndNoExternalOrBillingWrites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := New(f.db, f.audit)
	var before, after time.Time
	_ = f.db.Pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&before)
	f.seedProvider(t, "provider-real")
	c, err := r.BeginCapture(ctx, "provider-real", f.generation(t, "provider-real"), "region1")
	if err != nil {
		t.Fatal(err)
	}
	x := trusted()
	x.Capture = c
	x.Trust.Provider = Unreachable
	x.Trust.VMs = Untrusted
	observe(t, r, x)
	_ = f.db.Pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&after)
	if c.StartedAt.Before(before) || c.StartedAt.After(after) {
		t.Fatal("not DB clock", c.StartedAt, before, after)
	}
	rows, err := f.db.Pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	allowed := map[string]bool{"externalService": true, "auditEvent": true, "pveIncidentCapture": true, "pveIncidentState": true, "pveIncidentEvent": true}
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		if !allowed[name] {
			t.Fatal("unrelated cache/meter/billing table", name)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if _, err = r.List(ctx, "provider-real", Filter{Limit: 101}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	p, err := r.List(ctx, "provider-real", Filter{Kind: KindNode})
	if err != nil || len(p.Data) != 0 {
		t.Fatal("transport produced node down", p, err)
	}
	// No cloud/mail dependency exists in Repo; fixed reason codes cannot contain provider error bodies.
	p, err = r.List(ctx, "provider-real", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Data {
		if strings.Contains(s.Reason, "http") {
			t.Fatal(s)
		}
	}
}
