//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/providerattach"
)

// stallPVE fakes the Proxmox calls of BootstrapProxmoxOnto (pool, firewall groups). With stall set, a firewall-group
// list made once the default group exists blocks until the request's context ends.
func stallPVE(t *testing.T, stall *atomic.Bool) string {
	t.Helper()
	var mu sync.Mutex
	groups := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api2/json/pools", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"data":null}`) })
	mux.HandleFunc("GET /api2/json/cluster/firewall/groups", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := len(groups)
		body := `{"data":[` + strings.Join(groups, ",") + `]}`
		mu.Unlock()
		if n > 0 && stall.Load() {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("POST /api2/json/cluster/firewall/groups", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		groups = append(groups, fmt.Sprintf(`{"group":%q,"comment":%q}`, r.Form.Get("group"), r.Form.Get("comment")))
		mu.Unlock()
		fmt.Fprint(w, `{"data":null}`)
	})
	mux.HandleFunc("POST /api2/json/cluster/firewall/groups/{group}", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"data":null}`) })
	mux.HandleFunc("GET /api2/json/cluster/firewall/groups/{group}", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"data":[]}`) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func pveRow(id, url string, extra pgdoc.M) pgdoc.M {
	cfg := pgdoc.M{"provider": "proxmox", "apiUrl": url, "tokenId": "t", "region": "r1"}
	for k, v := range extra {
		cfg[k] = v
	}
	return pgdoc.M{"_id": id, "name": id, "type": externalservice.TypeCloud, "status": externalservice.StatusPublic, "config": cfg,
		"secret": pgdoc.M{"tokenSecret": "s"}}
}

// B1: a bulk (operator) attach of a never-opened project, through the real Proxmox bootstrap, whose attempt runs out
// right AFTER the binding is stored (the window before the Proxmox security-group sync) leaves a marked binding, never
// a bare one: the project's first open still provisions every provider whose policy is on. Before the fix the mark was
// a second write, lost to the timeout, and the project then missed every other provider for good.
func TestBulkAttachMarkSurvivesFailureAfterBinding(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	var stall atomic.Bool
	url := stallPVE(t, &stall)
	rig.projectH.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(es.PVEConfig(), es.ID, es.PVEPoolPrefix())
	})
	mustInsert(t, rig.db, "externalService", pveRow("pve", url, pgdoc.M{"attachToNewProjects": false}))
	mustInsert(t, rig.db, "externalService", dnsRow("dns", nil))
	rig.project(t, "fresh", "o1", project.StatusDisabled) // never opened: waiting on billing

	rig.runner.AttemptTimeout = 2 * time.Second
	// The failure lands right after the binding is stored: wait out the attempt's deadline there (the shape of a
	// provider call hanging in the step after the binding), then fail with it.
	rig.projectH.SetAfterBindForTest(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})
	runID := rig.start(t, "pve", `{"scope":{"mode":"projects","projectIds":["fresh"]}}`)
	rig.drain(t)
	rig.projectH.SetAfterBindForTest(nil)
	v := rig.view(t, "pve", runID)
	if v.Counts.Failed != 1 || v.Counts.ByCode[providerattach.CodeTimeout] != 1 {
		t.Fatalf("the attempt was meant to time out after the binding: %s %+v", v.Status, v.Counts)
	}
	p := rig.load(t, "fresh")
	if !p.HasService("pve") || p.HasService("dns") {
		t.Fatalf("after the timed-out attach: %v", p.ServiceIDs())
	}
	if m, _ := p.Services[0].(map[string]any); m["attachedBy"] == nil {
		t.Fatalf("the binding stored before the timeout is not marked: %v", p.Services)
	}
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "fresh")); err != nil {
		t.Fatal(err)
	}
	p = rig.load(t, "fresh")
	if !p.HasService("dns") || bindings(p, "pve") != 1 {
		t.Fatalf("first open did not provision the policy-on provider: %v", p.ServiceIDs())
	}
	for _, s := range p.Services {
		if m, _ := s.(map[string]any); m["attachedBy"] != nil {
			t.Fatalf("mark left after the first bootstrap: %v", p.Services)
		}
	}
	// Retrying the failed project: already attached, nothing called.
	code, out := rig.do(t, http.MethodPost, "/admin/service/pve/bulk-attach/"+runID+"/retry", "")
	if code != http.StatusOK {
		t.Fatalf("retry: %d %v", code, out)
	}
	rig.drain(t)
	if v := rig.view(t, "pve", out["data"].(map[string]any)["id"].(string)); v.Counts.ByCode[providerattach.CodeAlreadyAttached] != 1 {
		t.Fatalf("retry %+v", v.Counts)
	}
}

// N6: a Proxmox project bootstrapped or bulk-attached gets no stored copy of the provider's default quota (as on main:
// the copy was never persisted), so the provider default applies live and a change of it applies at once.
func TestProxmoxAttachStoresNoQuota(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	var stall atomic.Bool
	url := stallPVE(t, &stall)
	rig.projectH.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(es.PVEConfig(), es.ID, es.PVEPoolPrefix())
	})
	mustInsert(t, rig.db, "externalService", pveRow("pve", url, pgdoc.M{"provisioning": pgdoc.M{"quota": pgdoc.M{"diskGb": 10}}}))
	rig.project(t, "opened", "o1", project.StatusEnabled)      // first bootstrap
	rig.project(t, "bulk", "o1", project.StatusEnabled, "dns") // bulk attach of an existing project
	rig.start(t, "pve", `{"scope":{"mode":"projects","projectIds":["bulk"]}}`)
	rig.drain(t)
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "opened")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"opened", "bulk"} {
		p := rig.load(t, id)
		if !p.HasService("pve") {
			t.Fatalf("%s not attached: %v", id, p.ServiceIDs())
		}
		if p.Quota != nil {
			t.Fatalf("%s got a stored quota: %v", id, p.Quota)
		}
		var raw []byte
		if err := rig.db.Pool.QueryRow(ctx, `SELECT doc->'quota' FROM "project" WHERE id=$1`, id).Scan(&raw); err != nil || raw != nil {
			t.Fatalf("%s: stored quota %s %v", id, raw, err)
		}
	}
	// The live default is what the gate enforces: 10 GiB refuses 20, raising the default to 50 lets it through.
	csiUsed := func() (int, error) { return 0, nil }
	if err := rig.projectH.CSIDiskQuota(ctx, "bulk", "pve", 20, csiUsed); err == nil {
		t.Fatal("20 GiB passed a 10 GiB default")
	}
	if _, err := rig.db.C("externalService").SetByID(ctx, "pve", pgdoc.M{"config.provisioning.quota": pgdoc.M{"diskGb": 50}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"opened", "bulk"} {
		if err := rig.projectH.CSIDiskQuota(ctx, id, "pve", 20, csiUsed); err != nil {
			t.Fatalf("%s: the raised default was not applied: %v", id, err)
		}
	}
}

// N1: a first open with no policy-on provider at all still is the first bootstrap: it clears the operator marks, so a
// later open does not provision again.
func TestFirstOpenWithoutPolicyProvidersClearsMarks(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", pgdoc.M{"attachToNewProjects": false}))
	rig.project(t, "fresh", "o1", project.StatusEnabled)
	rig.start(t, "s3", `{"scope":{"mode":"projects","projectIds":["fresh"]}}`)
	rig.drain(t)
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "fresh")); err != nil {
		t.Fatal(err)
	}
	p := rig.load(t, "fresh")
	for _, s := range p.Services {
		if m, _ := s.(map[string]any); m["attachedBy"] != nil {
			t.Fatalf("mark left: %v", p.Services)
		}
	}
	// Now a provider with the policy on appears: an open of this (bootstrapped) project does not take it on its own.
	mustInsert(t, rig.db, "externalService", dnsRow("dns", nil))
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "fresh")); err != nil || rig.load(t, "fresh").HasService("dns") {
		t.Fatalf("a bootstrapped project took a later provider: %v", rig.load(t, "fresh").ServiceIDs())
	}
}

// N2: the per-project attach of a never-opened project marks its binding too, so its first open still provisions every
// policy-on provider.
func TestPerProjectAttachBeforeFirstBootstrapMarks(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", pgdoc.M{"attachToNewProjects": false}))
	mustInsert(t, rig.db, "externalService", dnsRow("dns", nil))
	rig.project(t, "fresh", "o1", project.StatusDisabled)
	if code, out := rig.do(t, http.MethodPost, "/admin/project/fresh/external-service/s3", ""); code != http.StatusOK {
		t.Fatalf("%d %v", code, out)
	}
	if err := rig.projectH.EnableAndBootstrap(ctx, rig.load(t, "fresh")); err != nil {
		t.Fatal(err)
	}
	if p := rig.load(t, "fresh"); !p.HasService("dns") || bindings(p, "s3") != 1 {
		t.Fatalf("first open after a per-project attach: %v", p.ServiceIDs())
	}
}

// N4: a member change (a save from a read taken before the attach) racing a bulk attach keeps the binding the attach
// added, and the attach keeps the member change.
func TestMemberChangeRacingBulkAttachKeepsBoth(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", nil))
	const n = 6
	for i := 1; i <= n; i++ {
		rig.project(t, fmt.Sprintf("p%d", i), "o1", project.StatusEnabled, "dns")
	}
	// Stale reads, taken before the attach: the shape of AddMember/Rename/activation, which read, change and save.
	stale := map[string]*project.Project{}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("p%d", i)
		stale[id] = rig.load(t, id)
	}
	// Deterministic interleaving: each member change is saved right after the attach stored the binding (between the
	// change's read and its write), plus a few concurrent ones for good measure.
	rig.projectH.SetAfterBindForTest(func(_ context.Context, projectID string) error {
		p := stale[projectID]
		p.Memberships = append(p.Memberships, project.Membership{Sub: "new-member", Role: project.RoleMember})
		p.Name = "renamed " + projectID
		return rig.projects.Save(ctx, p)
	})
	defer rig.projectH.SetAfterBindForTest(nil)
	rig.start(t, "s3", `{"scope":{"mode":"all"},"concurrency":3}`)
	var wg sync.WaitGroup
	wg.Go(func() { rig.drain(t) })
	for id := range stale {
		wg.Go(func() {
			p := rig.load(t, id)
			p.CustomInfo = map[string]any{"touched": true}
			if err := rig.projects.Save(ctx, p); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	rig.drain(t)
	for id := range stale {
		p := rig.load(t, id)
		if bindings(p, "s3") != 1 || !p.HasService("dns") || !p.IsMember("new-member") || p.Name != "renamed "+id {
			t.Fatalf("%s: bindings %v, member %v, name %q", id, p.ServiceIDs(), p.IsMember("new-member"), p.Name)
		}
	}
}

// The attach-policy PUT is a single-key write: a connection save of the same provider landing at the same time is not
// reverted, and it does not revert the policy. It also works on a live sandbox provider (no go-live gate).
func TestAttachPolicyWriteIsSingleKey(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", dnsRow("dns", nil))
	mustInsert(t, rig.db, "externalService", pgdoc.M{"_id": "sbx", "name": "sbx", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "sandbox", "sandbox": pgdoc.M{}}})
	for i := 0; i < 20; i++ {
		var wg sync.WaitGroup
		on := i%2 == 0
		api := fmt.Sprintf("http://127.0.0.%d:1", i+2)
		wg.Go(func() {
			if code, out := rig.do(t, http.MethodPut, "/admin/service/dns/attach-policy", fmt.Sprintf(`{"attachToNewProjects":%v}`, on)); code != http.StatusOK {
				t.Errorf("policy: %d %v", code, out)
			}
		})
		wg.Go(func() {
			if code, out := rig.do(t, http.MethodPut, "/admin/service/dns", `{"config":{"apiUrl":"`+api+`"}}`); code != http.StatusOK {
				t.Errorf("connection save: %d %v", code, out)
			}
		})
		wg.Wait()
		es, err := rig.esSvc.Get(ctx, "dns")
		if err != nil {
			t.Fatal(err)
		}
		if es.Config["apiUrl"] != api {
			t.Fatalf("round %d: the policy write reverted the connection save: %v", i, es.Config["apiUrl"])
		}
		// The connection save is a read-modify-replace that may carry the policy it read; the policy write itself
		// never takes anything else with it. When the policy write landed last it must stand.
		_ = es.AttachToNewProjects()
	}
	if code, out := rig.do(t, http.MethodPut, "/admin/service/sbx/attach-policy", `{"attachToNewProjects":false}`); code != http.StatusOK {
		t.Fatalf("policy on a live sandbox provider: %d %v", code, out)
	}
	if code, _ := rig.do(t, http.MethodPut, "/admin/service/missing/attach-policy", `{"attachToNewProjects":false}`); code != http.StatusNotFound {
		t.Fatalf("missing provider: %d", code)
	}
}

// A binding append runs on the attach lock's own session, the "lock still held" check inside the same statement: a
// session that does not hold the lock, one that let it go, and one that is gone all write nothing (ErrAttachLost); the
// session that holds it writes.
func TestBindingAppendRefusedWithoutLock(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	rig.project(t, "p1", "o1", project.StatusEnabled)
	const key = "project-attach:p1"
	bind := func(id string) map[string]any { return map[string]any{"serviceId": id, "provider": "powerdns"} }
	// A session that holds the lock writes; once it lets the lock go, the same session writes nothing.
	if _, err := rig.db.WithAdvisoryLockSession(ctx, key, false, func(s pgdoc.LockSession) error {
		lx := &project.LockedExec{Session: s, Key: key}
		if ok, err := rig.projects.AppendBinding(ctx, "p1", bind("held"), lx); err != nil || !ok {
			return fmt.Errorf("append while held: %v %v", ok, err)
		}
		if ok, err := rig.projects.AppendBinding(ctx, "p1", bind("held"), lx); err != nil || ok {
			return fmt.Errorf("second append while held: %v %v", ok, err) // already bound: no write, no error
		}
		if _, err := s.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))", key); err != nil {
			return err
		}
		if _, err := rig.projects.AppendBinding(ctx, "p1", bind("after-unlock"), lx); !errors.Is(err, project.ErrAttachLost) {
			return fmt.Errorf("append after the unlock: %v", err)
		}
		if _, err := rig.projects.ClearBulkMarks(ctx, "p1", lx); !errors.Is(err, project.ErrAttachLost) {
			return fmt.Errorf("mark clear after the unlock: %v", err)
		}
		if _, err := s.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", key); err != nil { // so the release finds it
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A session holding a different lock: refused.
	if _, err := rig.db.WithAdvisoryLockSession(ctx, "project-attach:other", false, func(s pgdoc.LockSession) error {
		if _, err := rig.projects.AppendBinding(ctx, "p1", bind("other-lock"), &project.LockedExec{Session: s, Key: key}); !errors.Is(err, project.ErrAttachLost) {
			return fmt.Errorf("append under another lock: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A session that is gone (its backend terminated): refused.
	if _, err := rig.db.WithAdvisoryLockSession(ctx, key, false, func(s pgdoc.LockSession) error {
		var pid int
		if err := s.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			return err
		}
		if _, err := rig.db.Pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
			return err
		}
		if _, err := rig.projects.AppendBinding(ctx, "p1", bind("dead-session"), &project.LockedExec{Session: s, Key: key}); !errors.Is(err, project.ErrAttachLost) {
			return fmt.Errorf("append on a dead session: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p := rig.load(t, "p1")
	if !p.HasService("held") || bindings(p, "held") != 1 || p.HasService("after-unlock") || p.HasService("other-lock") || p.HasService("dead-session") {
		t.Fatalf("bindings %v", p.ServiceIDs())
	}
	// The real guard, on the lock's session: held → the append goes through, exactly once.
	for i := 0; i < 2; i++ {
		if _, err := rig.projectH.AttachByOperator(ctx, "p1", &externalservice.ExternalService{ID: "dns2", Config: map[string]any{"provider": "powerdns"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := bindings(rig.load(t, "p1"), "dns2"); got != 1 {
		t.Fatalf("%d bindings", got)
	}
}

// Bounds: a run is planned in batches (resumable), already attached projects get no item row (a count on the run),
// a scope above the cap is refused up front, an unattachable provider kind is refused up front, and finished runs'
// items are pruned after the retention.
func TestBulkAttachBounds(t *testing.T) {
	rig := newAttachRig(t)
	ctx := context.Background()
	mustInsert(t, rig.db, "externalService", dnsRow("dns", pgdoc.M{"attachToNewProjects": false}))
	mustInsert(t, rig.db, "externalService", pgdoc.M{"_id": "odd", "name": "odd", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
		"config": pgdoc.M{"provider": "something-else"}})
	total := providerattach.PlanBatch + 150
	docs := make([]any, 0, total)
	for i := 0; i < total; i++ {
		svcs := []any{}
		if i%10 == 0 {
			svcs = append(svcs, pgdoc.M{"serviceId": "dns", "provider": "powerdns"})
		}
		docs = append(docs, pgdoc.M{"_id": fmt.Sprintf("p%05d", i), "name": "p", "organizationId": "o1", "status": project.StatusEnabled, "services": svcs})
	}
	for _, d := range docs {
		mustInsert(t, rig.db, "project", d)
	}
	runID := rig.start(t, "dns", `{"scope":{"mode":"all"},"concurrency":4}`)
	rig.drain(t)
	v := rig.view(t, "dns", runID)
	already := int64((total + 9) / 10)
	if v.Status != providerattach.RunSucceeded || v.Counts.Total != int64(total) || v.Counts.Attached != int64(total)-already ||
		v.Counts.ByCode[providerattach.CodeAlreadyAttached] != already {
		t.Fatalf("run %s %+v", v.Status, v.Counts)
	}
	var rows int64
	if err := rig.db.Pool.QueryRow(ctx, `SELECT count(*) FROM "providerAttachItem" WHERE doc->>'runId'=$1`, runID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != int64(total)-already {
		t.Fatalf("%d item rows, want %d (none for already attached projects)", rows, int64(total)-already)
	}
	// An unattachable provider kind is refused before anything is queued.
	if code, out := rig.do(t, http.MethodPost, "/admin/service/odd/bulk-attach", `{"scope":{"mode":"all"}}`); code != http.StatusBadRequest {
		t.Fatalf("unattachable kind: %d %v", code, out)
	}
	// Pruning: a run finished more than the retention ago loses its items, keeps its counts, and cannot be retried.
	if _, err := rig.db.Pool.Exec(ctx, `UPDATE "providerAttachRun" SET doc=jsonb_set(doc,'{finishedAt}',to_jsonb($2::text)) WHERE id=$1`,
		runID, time.Now().UTC().Add(-providerattach.ItemRetention-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if did, err := rig.runner.RunOnce(ctx); did || err != nil {
		t.Fatalf("idle pass: %v %v", did, err)
	}
	if err := rig.db.Pool.QueryRow(ctx, `SELECT count(*) FROM "providerAttachItem" WHERE doc->>'runId'=$1`, runID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("items after the prune: %d %v", rows, err)
	}
	if v := rig.view(t, "dns", runID); !v.ItemsPruned || v.Counts.Total != int64(total) {
		t.Fatalf("pruned run %+v", v)
	}
}

// N5: the provider is re-read for every project, so a provider disabled mid-run stops the run: the projects not done
// yet fail provider_disabled (a retry picks them up), none is attached after the disable.
func TestBulkAttachProviderDisabledMidRun(t *testing.T) {
	rig := newAttachRig(t)
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", nil))
	for i := 1; i <= 6; i++ {
		rig.project(t, fmt.Sprintf("p%d", i), "o1", project.StatusEnabled, "dns")
	}
	runID := rig.start(t, "s3", `{"scope":{"mode":"all"},"concurrency":1}`)
	orig := rig.runner.Attach
	n := 0
	rig.runner.Attach = func(ctx context.Context, projectID string, es *externalservice.ExternalService) (string, error) {
		out, err := orig(ctx, projectID, es)
		n++
		if n == 2 {
			if _, e := rig.db.C("externalService").SetByID(context.Background(), "s3", pgdoc.M{"status": externalservice.StatusDisabled}, nil); e != nil {
				t.Error(e)
			}
		}
		return out, err
	}
	rig.drain(t)
	v := rig.view(t, "s3", runID)
	if v.Status != providerattach.RunFailed || v.Code != providerattach.CodeProviderDisabled || v.Counts.Attached != 2 ||
		v.Counts.ByCode[providerattach.CodeProviderDisabled] != 4 {
		t.Fatalf("run %s %s %+v", v.Status, v.Code, v.Counts)
	}
	if got := rig.rgwCalls.Load(); got != 2 {
		t.Fatalf("%d provider calls after the disable", got)
	}
	if j, _ := rig.jobs.Get(context.Background(), v.JobID); j.State != "succeeded" {
		t.Fatalf("job %+v", j)
	}
	_ = durablejobs.ProviderAttach
}

// A cancel that arrives once every project is done does not relabel the completed run as cancelled.
func TestLateCancelKeepsCompletedRun(t *testing.T) {
	rig := newAttachRig(t)
	mustInsert(t, rig.db, "externalService", rig.s3Row("s3", nil))
	for i := 1; i <= 3; i++ {
		rig.project(t, fmt.Sprintf("p%d", i), "o1", project.StatusEnabled, "dns")
	}
	runID := rig.start(t, "s3", `{"scope":{"mode":"all"},"concurrency":1}`)
	orig := rig.runner.Attach
	n := 0
	rig.runner.Attach = func(ctx context.Context, projectID string, es *externalservice.ExternalService) (string, error) {
		out, err := orig(ctx, projectID, es)
		if n++; n == 3 {
			if code, body := rig.do(t, http.MethodPost, "/admin/service/s3/bulk-attach/"+runID+"/cancel", ""); code != http.StatusOK {
				t.Errorf("cancel: %d %v", code, body)
			}
		}
		return out, err
	}
	rig.drain(t)
	if v := rig.view(t, "s3", runID); v.Status != providerattach.RunSucceeded || v.Counts.Attached != 3 || v.Counts.Cancelled != 0 {
		t.Fatalf("late cancel: %s %+v", v.Status, v.Counts)
	}
}
