package templateupdates

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// lifecycleFake is the smoke fake with the one read it lacks for LifecyclePVE.
type lifecycleFake struct{ *smokePVE }

func (lifecycleFake) VMIDFree(context.Context, int) (bool, error) { return false, nil }

type publishAllFixture struct {
	t   *testing.T
	s   *Service
	db  *pgdoc.DB
	pv  *smokePVE
	reg *fakeRegistry
	p   VMPayload
	in  PublishAllInput
	net error // what SmokeNet answers
}

// publishAllSetup: a provider of six nodes and the golden Kubernetes artifact in the registry.
func publishAllSetup(t *testing.T) *publishAllFixture {
	t.Helper()
	db, jobs := pgFixture(t)
	ctx := context.Background()
	raw, sums, disk := golden(t, "k8s")
	p := k8sPayload()
	p.ServiceID, p.ImportStorage, p.DiskStorage = "svc-pve", "nas-import", "local-zfs"
	reg := &fakeRegistry{}
	reg.add(p.Package, p.Digest, "2026-11-12T02:00:00Z", p.Tag, artifactManifest(raw, sums, disk, false), raw, sums)
	s := &Service{Token: func(context.Context) (string, string, error) { return "ghp_x", "i", nil }, Jobs: jobs, DB: db,
		Receipts: PGReceipts{DB: db}, Smokes: PGSmokes{DB: db}, VMEnabled: true, newRegistry: func(string) (Registry, error) { return reg, nil }}
	if err := s.Smokes.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	f := &publishAllFixture{t: t, s: s, db: db, pv: newSmokePVE(), reg: reg, p: p}
	f.in = PublishAllInput{ServiceID: p.ServiceID, Package: p.Package, Digest: p.Digest, ConfirmDigest: p.Digest, ImportStorage: "nas-import",
		DiskStorage: func(string) string { return "local-zfs" },
		SmokeNet: func(context.Context, string, string) (SmokeNetwork, error) {
			return SmokeNetwork{Bridge: "vmbr0", VLAN: 29}, f.net
		}}
	for _, n := range []string{"pve1", "pve2", "pve3", "pve4", "pve5", "pve6"} {
		f.in.Nodes = append(f.in.Nodes, pve.Node{Node: n, Status: "online"})
		if n != "pve6" { // pve6 has no import storage
			f.in.Options = append(f.in.Options, PlacementOption{Node: n, ImportStorages: []string{"nas-import"}, DiskStorages: []string{"local-zfs"}})
		}
	}
	return f
}

// imported records a finished import of the artifact on node and its template.
func (f *publishAllFixture) imported(node string, vmid int, published bool) string {
	f.t.Helper()
	p := f.p
	p.Node = node
	tags := TemplateTags(p)
	if published {
		tags = publishedTags(tags, p)
	}
	f.pv.vms[vmid] = &smokeVM{node: node, template: true, cfg: pve.VMConfig{"name": TemplateName(p), "tags": tags, "pool": pve.TemplatePool}}
	id := uuid.NewString()
	if err := f.s.Receipts.Save(context.Background(), &Receipt{ID: id, Kind: "vm", Payload: p, Step: StepSucceeded, VMID: vmid, CreatedAt: time.Now().UTC()}); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// smoked records a finished smoke test of an import's template.
func (f *publishAllFixture) smoked(importID, node string, vmid int, result, code string) {
	f.t.Helper()
	now := time.Now().UTC()
	err := f.s.Smokes.SaveSmoke(context.Background(), &SmokeReceipt{ID: uuid.NewString(), ImportID: importID, Step: SmokeFinished, Result: result, Code: code,
		Cleaned: true, TemplateConfigDigest: "d", CreatedAt: now, FinishedAt: &now,
		Payload: SmokePayload{ServiceID: f.p.ServiceID, Node: node, TemplateVMID: vmid, Digest: f.p.Digest}})
	if err != nil {
		f.t.Fatal(err)
	}
}

// pipeline adds a published node image the build pipeline made (not an import).
func (f *publishAllFixture) pipeline(node string, vmid int, k8s string) {
	f.pv.vms[vmid] = &smokeVM{node: node, template: true, cfg: pve.VMConfig{"name": "k8s-node", "pool": pve.TemplatePool,
		"tags": "proxcloud;pc-k8s;pc-k8s-" + strings.ReplaceAll(k8s, ".", "-") + ";pc-k8s-os-ubuntu-2604;pc-v-202610010000"}}
}

func (f *publishAllFixture) run(act bool) *PublishAllReport {
	f.t.Helper()
	rep, err := f.s.PublishAll(context.Background(), lifecycleFake{f.pv}, f.in, "admin", act)
	if err != nil {
		f.t.Fatal(err)
	}
	return rep
}

func (f *publishAllFixture) jobs() int {
	f.t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM durable_job`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func states(rep *PublishAllReport) string {
	out := []string{}
	for _, n := range rep.Nodes {
		out = append(out, n.Node+"="+n.State)
	}
	return strings.Join(out, " ")
}

// Six nodes in mixed states: a read changes nothing; an advance publishes what passed and starts
// exactly one job; a second advance is idempotent; a node whose smoke test failed keeps its older
// published copy in the versions line; a node that only has another version is reported missing.
func TestPGPublishAllMixedNodes(t *testing.T) {
	f := publishAllSetup(t)
	f.imported("pve1", 9210, true)
	ready := f.imported("pve2", 9211, false)
	f.smoked(ready, "pve2", 9211, SmokePass, "")
	staged := f.imported("pve3", 9212, false)
	failed := f.imported("pve5", 9214, false)
	f.smoked(failed, "pve5", 9214, SmokeFail, "cloud_init_failed")
	f.pipeline("pve5", 9100, "1.35.10") // an older published copy of the same version
	f.pipeline("pve6", 9101, "1.34.8")  // only another version

	rep := f.run(false)
	if got := states(rep); got != "pve1=published pve2=ready_to_publish pve3=staged pve4=missing pve5=smoke_failed pve6=skipped" {
		t.Fatalf("read: %s", got)
	}
	if rep.Next != "advance" || rep.Complete || len(rep.Actions) != 0 || f.jobs() != 0 || f.pv.mutations != 0 {
		t.Fatalf("a read acted: %+v jobs=%d mutations=%d", rep, f.jobs(), f.pv.mutations)
	}
	if rep.Nodes[4].Code != "cloud_init_failed" || !strings.Contains(rep.Nodes[5].Message, "not both active") {
		t.Fatalf("reasons: %+v", rep.Nodes)
	}

	rep = f.run(true)
	if got := states(rep); got != "pve1=published pve2=published pve3=smoke_queued pve4=missing pve5=smoke_failed pve6=skipped" {
		t.Fatalf("first advance: %s", got)
	}
	if f.jobs() != 1 || rep.Next != "wait" || len(rep.Actions) != 2 {
		t.Fatalf("first advance must publish pve2 and queue one smoke test: jobs=%d %+v", f.jobs(), rep)
	}
	if rep.VersionsLine != `"1.35.10": "pve1=9210,pve2=9211,pve5=9100"` || rep.KubernetesVersion != "1.35.10" {
		t.Fatalf("versions line %q", rep.VersionsLine)
	}
	if rep.Complete || len(rep.NodesMissing) != 1 || rep.NodesMissing[0] != "pve6" {
		t.Fatalf("complete=%v missing=%v", rep.Complete, rep.NodesMissing)
	}
	if !isPublished(f.pv.vms[9211].cfg.Str("tags"), "kubernetes") || isPublished(f.pv.vms[9214].cfg.Str("tags"), "kubernetes") {
		t.Fatal("pve2 must be published and the failed pve5 copy must not")
	}

	// Again while the smoke test runs: nothing is published twice, no second job.
	mut := f.pv.mutations
	rep = f.run(true)
	if got := states(rep); got != "pve1=published pve2=published pve3=smoke_running pve4=missing pve5=smoke_failed pve6=skipped" {
		t.Fatalf("second advance: %s", got)
	}
	if f.jobs() != 1 || len(rep.Actions) != 0 || f.pv.mutations != mut || rep.Next != "wait" {
		t.Fatalf("second advance acted: jobs=%d %+v", f.jobs(), rep)
	}

	// The smoke test of pve3 passes: it is published, and only now is the import of pve4 queued.
	ctx := context.Background()
	rec, _ := f.s.Receipts.Load(ctx, staged)
	sm, _ := f.s.Smokes.LoadSmoke(ctx, rec.Smoke.JobID)
	now := time.Now().UTC()
	sm.Step, sm.Result, sm.Cleaned, sm.TemplateConfigDigest, sm.FinishedAt = SmokeFinished, SmokePass, true, "d", &now
	if err := f.s.Smokes.SaveSmoke(ctx, sm); err != nil {
		t.Fatal(err)
	}
	rep = f.run(true)
	if got := states(rep); got != "pve1=published pve2=published pve3=published pve4=import_queued pve5=smoke_failed pve6=skipped" {
		t.Fatalf("third advance: %s", got)
	}
	if f.jobs() != 2 || rep.Next != "wait" {
		t.Fatalf("third advance: jobs=%d next=%s", f.jobs(), rep.Next)
	}
	if rep = f.run(true); f.jobs() != 2 || rep.Nodes[3].State != NodeImporting {
		t.Fatalf("an import in flight was queued again: jobs=%d %s", f.jobs(), states(rep))
	}
}

// A smoke network that is unset is reported for that node and recorded, the import of the next node
// still starts, and the smoke test is not tried again by a later advance.
func TestPGPublishAllSmokeNetworkUnset(t *testing.T) {
	f := publishAllSetup(t)
	f.in.Nodes, f.in.Options = f.in.Nodes[:2], f.in.Options[:2]
	f.imported("pve1", 9210, false)
	f.net = ErrSmokeNetworkUnset
	rep := f.run(true)
	if got := states(rep); got != "pve1=smoke_not_started pve2=import_queued" {
		t.Fatalf("advance: %s", got)
	}
	var le *LifecycleError
	if !errors.As(ErrSmokeNetworkUnset, &le) || rep.Nodes[0].Code != le.Code || f.jobs() != 1 {
		t.Fatalf("node %+v jobs=%d", rep.Nodes[0], f.jobs())
	}
	f.net = nil
	if _, err := f.db.Pool.Exec(context.Background(), `UPDATE durable_job SET state='dead',finished_at=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	rep = f.run(true)
	if rep.Nodes[0].State != NodeSmokeNotStarted || rep.Nodes[1].State != NodeImportFailed || f.jobs() != 1 || rep.Next != "done" {
		t.Fatalf("a failed step was repeated: %s jobs=%d next=%s", states(rep), f.jobs(), rep.Next)
	}
}

// Refusals: a withdrawn digest, an unconfirmed digest, the feature flag, and a copy the portal did
// not import, which is never imported over.
func TestPGPublishAllRefusals(t *testing.T) {
	f := publishAllSetup(t)
	ctx := context.Background()
	f.in.Nodes, f.in.Options = f.in.Nodes[:1], f.in.Options[:1]
	f.pv.vms[9300] = &smokeVM{node: "pve1", template: true, cfg: pve.VMConfig{"name": "by-hand", "pool": pve.TemplatePool, "tags": TagSource + digestPrefix(f.p.Digest)}}
	rep := f.run(true)
	if rep.Nodes[0].State != NodePresentUnmanaged || rep.Nodes[0].VMID != 9300 || f.jobs() != 0 || rep.Next != "done" {
		t.Fatalf("unmanaged copy: %+v jobs=%d", rep, f.jobs())
	}
	bad := f.in
	bad.ConfirmDigest = ""
	if _, err := f.s.PublishAll(ctx, lifecycleFake{f.pv}, bad, "admin", true); err == nil {
		t.Fatal("an unconfirmed digest was accepted")
	}
	if _, err := f.s.Receipts.Block(ctx, BlockedDigest{Digest: f.p.Digest, Reason: "bad image", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PublishAll(ctx, lifecycleFake{f.pv}, f.in, "admin", false); !errors.Is(err, ErrBlocklisted) {
		t.Fatalf("withdrawn digest: %v", err)
	}
	f.s.VMEnabled = false
	if _, err := f.s.PublishAll(ctx, lifecycleFake{f.pv}, f.in, "admin", false); !errors.Is(err, errVMDisabled) {
		t.Fatalf("flag off: %v", err)
	}
}

// downNodePVE reads the cluster as Proxmox shows it while a node is down: the VM rows of that node are
// still listed, and nothing in them but the VMID and the node counts (pve.markNodeDown).
type downNodePVE struct {
	lifecycleFake
	down string
}

func (d downNodePVE) ClusterResources(ctx context.Context, kind string) ([]pve.ClusterResource, error) {
	rows, err := d.lifecycleFake.ClusterResources(ctx, kind)
	for i, r := range rows {
		if r.Node == d.down {
			rows[i] = pve.ClusterResource{Type: "qemu", VMID: r.VMID, Node: r.Node, Status: "unknown", NodeDown: true}
		}
	}
	return rows, err
}

// A node that is down while it holds a published copy: the versions line cannot name it, so the report
// is not complete and the dialog warns before the line is copied. A line without that node would have
// each cluster's workers there pruned at its next edit.
func TestPGPublishAllNodeDown(t *testing.T) {
	f := publishAllSetup(t)
	f.in.Nodes, f.in.Options = f.in.Nodes[:4], f.in.Options[:4]
	f.imported("pve1", 9210, true)
	f.imported("pve2", 9211, true)
	f.imported("pve3", 9212, true)
	f.in.Nodes[2].Status, f.in.Nodes[3].Status = "offline", "offline" // pve3 holds a copy, pve4 holds none
	rep, err := f.s.PublishAll(context.Background(), downNodePVE{lifecycleFake{f.pv}, "pve3"}, f.in, "admin", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := states(rep); got != "pve1=published pve2=published pve3=skipped pve4=skipped" {
		t.Fatalf("states: %s", got)
	}
	if rep.Nodes[2].Code != NodeDownCode || rep.Nodes[3].Code != NodeDownCode || rep.Nodes[2].VMID != 9212 {
		t.Fatalf("down nodes: %+v", rep.Nodes[2:])
	}
	if rep.Complete || rep.Next != "done" || rep.VersionsLine != `"1.35.10": "pve1=9210,pve2=9211"` || f.jobs() != 0 {
		t.Fatalf("a line without the down node must not be complete: %+v jobs=%d", rep, f.jobs())
	}
	// The node is back: the same call is complete and the line has it.
	f.in.Nodes, f.in.Options = f.in.Nodes[:3], f.in.Options[:3]
	f.in.Nodes[2].Status = "online"
	if rep = f.run(true); !rep.Complete || rep.VersionsLine != `"1.35.10": "pve1=9210,pve2=9211,pve3=9212"` {
		t.Fatalf("node back: %+v", rep)
	}
	// A node skipped for its storage, not for being down, does not hold the report open.
	f.in.Nodes = append(f.in.Nodes, pve.Node{Node: "pve6", Status: "online"})
	if rep = f.run(true); !rep.Complete || rep.Nodes[3].State != NodeSkipped || rep.Nodes[3].Code != "" {
		t.Fatalf("storage skip: %+v", rep)
	}
}

// An advance that published a node and then could not start the next import (GitHub rate limit): the
// call answers with the report, so the publish is shown and audited, and no job is started. With
// nothing done first, the same failure is the call's error.
func TestPGPublishAllKeepsWhatItDid(t *testing.T) {
	f := publishAllSetup(t)
	f.in.Nodes, f.in.Options = f.in.Nodes[:3], f.in.Options[:3]
	ready := f.imported("pve1", 9210, false)
	f.smoked(ready, "pve1", 9210, SmokePass, "")
	f.reg.listErr = &RateLimitError{Limit: 60, Resource: "core"}
	rep, err := f.s.PublishAll(context.Background(), lifecycleFake{f.pv}, f.in, "admin", true)
	if err != nil {
		t.Fatalf("the publish of pve1 is lost behind the error of the import: %v", err)
	}
	if got := states(rep); got != "pve1=published pve2=missing pve3=missing" {
		t.Fatalf("states: %s", got)
	}
	if len(rep.Actions) != 1 || rep.Actions[0] != "published pve1/9210" || !isPublished(f.pv.vms[9210].cfg.Str("tags"), "kubernetes") {
		t.Fatalf("actions %v", rep.Actions)
	}
	if rep.Error == nil || rep.Error.Code != "rate_limited" || !strings.Contains(rep.Error.Message, "rate limit") || rep.Next != "advance" || rep.Complete || f.jobs() != 0 {
		t.Fatalf("error %+v next=%s complete=%v jobs=%d", rep.Error, rep.Next, rep.Complete, f.jobs())
	}
	// The second call has nothing to publish first: the failure is its answer, as for a first import.
	var rl *RateLimitError
	if rep, err = f.s.PublishAll(context.Background(), lifecycleFake{f.pv}, f.in, "admin", true); !errors.As(err, &rl) || rep != nil {
		t.Fatalf("nothing done, registry failing: %+v, %v", rep, err)
	}
	// The registry answers again: one import starts.
	f.reg.listErr = nil
	if rep = f.run(true); states(rep) != "pve1=published pve2=import_queued pve3=missing" || rep.Error != nil || f.jobs() != 1 {
		t.Fatalf("after the limit: %s %+v jobs=%d", states(rep), rep.Error, f.jobs())
	}
}
