//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
)

// reconcileProv returns a settable list — stands in for a live cloud provider so Reconcile's
// gates (create/update/skip/delete-of-vanished/recreate-guard) are tested against real Postgres.
type reconcileProv struct {
	typ  string
	list []cloud.CloudResource
}

func (p *reconcileProv) Type() string { return p.typ }
func (p *reconcileProv) List(context.Context) ([]cloud.CloudResource, error) {
	return p.list, nil
}

func rcRes(extID string, data map[string]any) cloud.CloudResource {
	return cloud.CloudResource{ExternalID: extID, ProjectID: "proj1", Type: cloud.TypeServer, Region: "RegionOne", Data: data}
}

// TestCloudReconcile exercises providers.Reconcile against real Postgres.
func TestCloudReconcile(t *testing.T) {
	ctx := context.Background()
	repo := cloud.NewRepo(freshPG(t))
	p := &reconcileProv{typ: cloud.TypeServer}
	base := time.Now().UTC().Truncate(time.Millisecond)
	t1 := base
	t2 := base.Add(time.Minute)
	t3 := base.Add(2 * time.Minute)

	// round 1: A,B new → 2 created.
	p.list = []cloud.CloudResource{rcRes("A", map[string]any{"name": "a1"}), rcRes("B", map[string]any{"name": "b1"})}
	st, err := providers.Reconcile(ctx, p, repo, "svc1", t1)
	if err != nil {
		t.Fatalf("r1: %v", err)
	}
	if st.Created != 2 || st.Updated != 0 || st.Deleted != 0 {
		t.Fatalf("r1 stats: %+v", st)
	}

	// round 2: A changed, B same → 1 updated (A), B skipped (isNeededToUpdate).
	p.list = []cloud.CloudResource{rcRes("A", map[string]any{"name": "a2"}), rcRes("B", map[string]any{"name": "b1"})}
	st, err = providers.Reconcile(ctx, p, repo, "svc1", t2)
	if err != nil {
		t.Fatalf("r2: %v", err)
	}
	if st.Created != 0 || st.Updated != 1 || st.Deleted != 0 {
		t.Fatalf("r2 stats: %+v (want updated=1)", st)
	}
	if a, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "A"); a == nil || a.Data["name"] != "a2" {
		t.Fatalf("r2: A not updated: %+v", a)
	}

	// round 3: B vanished from cloud → 1 deleted (delete-of-vanished).
	p.list = []cloud.CloudResource{rcRes("A", map[string]any{"name": "a2"})}
	st, err = providers.Reconcile(ctx, p, repo, "svc1", t3)
	if err != nil {
		t.Fatalf("r3: %v", err)
	}
	if st.Deleted != 1 || st.Created != 0 || st.Updated != 0 {
		t.Fatalf("r3 stats: %+v (want deleted=1)", st)
	}
	if b, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "B"); b != nil {
		t.Fatalf("r3: B not deleted: %+v", b)
	}

	// recreate guard: a resource the user deleted AFTER the sync snapshot is not resurrected.
	delAt := base.Add(10 * time.Minute)
	c := rcRes("C", map[string]any{"name": "c1"})
	c.ServiceID = "svc1"
	cc, _ := repo.Insert(ctx, &c)
	if err := repo.DeleteAndArchive(ctx, cc, delAt); err != nil { // user delete at delAt
		t.Fatalf("seed C delete: %v", err)
	}
	snapshot := base.Add(5 * time.Minute) // BEFORE the user deletion
	p.list = []cloud.CloudResource{rcRes("C", map[string]any{"name": "c1"})}
	st, err = providers.Reconcile(ctx, p, repo, "svc1", snapshot)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if st.Created != 0 {
		t.Fatalf("guard: C should NOT be recreated (deleted after snapshot), stats: %+v", st)
	}
	if got, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "C"); got != nil {
		t.Fatalf("guard: C resurrected: %+v", got)
	}
}

// reconcileProvScoped is a ProjectScoped fake provider — Reconcile scopes its delete-of-vanished
// scan to (serviceId, projectId, type) so it never touches another project's cached resources.
type reconcileProvScoped struct {
	typ       string
	projectID string
	list      []cloud.CloudResource
}

func (p *reconcileProvScoped) Type() string      { return p.typ }
func (p *reconcileProvScoped) ProjectID() string { return p.projectID }
func (p *reconcileProvScoped) List(context.Context) ([]cloud.CloudResource, error) {
	return p.list, nil
}

// TestCloudReconcileProjectScopedNoLeak proves the audit §5 cross-project delete fix: two projects
// share serviceId svc1 + type SERVER; project A's sync must NOT delete-archive project B's cached
// resource, yet must still delete A's own vanished resource.
func TestCloudReconcileProjectScopedNoLeak(t *testing.T) {
	ctx := context.Background()
	repo := cloud.NewRepo(freshPG(t))
	base := time.Now().UTC().Truncate(time.Millisecond)
	past := base.Add(-time.Hour)

	// Seed A1 (projA) + B2 (projB) — SAME serviceId, SAME type.
	seed := func(ext, proj string) {
		r := cloud.CloudResource{
			ServiceID: "svc1", ExternalID: ext, ProjectID: proj, Type: cloud.TypeServer,
			Region: "RegionOne", Data: map[string]any{"name": ext}, CreatedAt: &past, UpdatedAt: &past,
		}
		if _, err := repo.Insert(ctx, &r); err != nil {
			t.Fatalf("seed %s: %v", ext, err)
		}
	}
	seed("A1", "projA")
	seed("B2", "projB")

	// Round 1: projA's live list still has A1 (unchanged). The delete-scan MUST be scoped to projA,
	// so B2 (projB) is never even considered → survives.
	pa := &reconcileProvScoped{typ: cloud.TypeServer, projectID: "projA",
		list: []cloud.CloudResource{{ServiceID: "svc1", ExternalID: "A1", ProjectID: "projA", Type: cloud.TypeServer, Region: "RegionOne", Data: map[string]any{"name": "A1"}}}}
	st, err := providers.Reconcile(ctx, pa, repo, "svc1", base)
	if err != nil {
		t.Fatalf("r1: %v", err)
	}
	if st.Deleted != 0 {
		t.Fatalf("r1: deleted=%d, want 0 (must NOT touch projB)", st.Deleted)
	}
	if b, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "B2"); b == nil {
		t.Fatal("r1: projB's B2 was wrongly deleted (cross-project leak)")
	}

	// Round 2: A1 vanished from projA's cloud → A1 deleted (within-project deletion still works),
	// B2 STILL survives.
	pa.list = nil
	st, err = providers.Reconcile(ctx, pa, repo, "svc1", base.Add(time.Minute))
	if err != nil {
		t.Fatalf("r2: %v", err)
	}
	if st.Deleted != 1 {
		t.Fatalf("r2: deleted=%d, want 1 (A1 vanished)", st.Deleted)
	}
	if a, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "A1"); a != nil {
		t.Fatal("r2: A1 should be deleted")
	}
	if b, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "B2"); b == nil {
		t.Fatal("r2: projB's B2 still must survive")
	}
}

// deletableProv marks resources with data.dead=true as shouldBeDeleted (stands in for the server
// provider's DELETED-status predicate).
type deletableProv struct{ reconcileProv }

func (p *deletableProv) ShouldBeDeleted(cr *cloud.CloudResource) bool {
	dead, _ := cr.Data["dead"].(bool)
	return dead
}

// TestCloudReconcileShouldBeDeleted: a cached resource the provider declares terminal is
// delete-archived even though the live list still contains it (shouldBeDeleted gate).
func TestCloudReconcileShouldBeDeleted(t *testing.T) {
	ctx := context.Background()
	repo := cloud.NewRepo(freshPG(t))
	base := time.Now().UTC().Truncate(time.Millisecond)
	past := base.Add(-time.Hour)

	seed := cloud.CloudResource{
		ServiceID: "svc1", ExternalID: "D1", ProjectID: "proj1", Type: cloud.TypeServer,
		Region: "RegionOne", Data: map[string]any{"name": "d1", "dead": true}, CreatedAt: &past, UpdatedAt: &past,
	}
	if _, err := repo.Insert(ctx, &seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := &deletableProv{reconcileProv{typ: cloud.TypeServer,
		list: []cloud.CloudResource{{ServiceID: "svc1", ExternalID: "D1", ProjectID: "proj1", Type: cloud.TypeServer, Region: "RegionOne", Data: map[string]any{"name": "d1", "dead": true}}}}}
	st, err := providers.Reconcile(ctx, p, repo, "svc1", base)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.Deleted != 1 || st.Updated != 0 {
		t.Fatalf("stats = %+v, want deleted=1 (gate fired before update)", st)
	}
	if got, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "D1"); got != nil {
		t.Fatal("terminal resource must be delete-archived despite being live-listed")
	}
}
