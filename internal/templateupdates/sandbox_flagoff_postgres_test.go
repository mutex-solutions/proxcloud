package templateupdates

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// jobScopedCluster lists only the worker pods of the asked job (the real cluster selects them by job label), so one
// job's cleanup can never reach another job's pod in these tests.
type jobScopedCluster struct{ *fakeCluster }

func (c jobScopedCluster) ListMirrorPods(ctx context.Context, short string) ([]sandbox.MirrorPod, error) {
	all, err := c.fakeCluster.ListMirrorPods(ctx, short)
	var out []sandbox.MirrorPod
	for _, p := range all {
		if p.JobShort == short {
			out = append(out, p)
		}
	}
	return out, err
}

// sbxEnqueuePG enqueues one sandbox job of p with its receipt (as enqueueSandbox does) and returns its id.
func sbxEnqueuePG(t *testing.T, db *pgdoc.DB, jobs *durablejobs.Store, store PGSandbox, p SandboxPayload, key string) string {
	t.Helper()
	raw, _ := json.Marshal(p)
	var id string
	if err := db.WithTx(context.Background(), func(tc context.Context) error {
		var err error
		if id, err = jobs.EnqueueTx(tc, durablejobs.TemplateUpdateSandbox, key, raw, 3); err != nil {
			return err
		}
		return store.Save(tc, &SandboxReceipt{ID: id, Kind: "sandbox", Payload: p, Step: SbxQueued, RequestedBy: "op-1", CreatedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// B1 (opt-in REAL PostgreSQL, TEMPLATEUPDATES_TEST_POSTGRES): with PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED off
// (CleanupOnly) a sandbox job in flight is not stranded. A queued job, and a running one whose lease expired, are
// dead-lettered as feature_disabled with their cleanup run (worker pods — and with them their Secrets — deleted, the
// pin of a prepull switch that never landed restored), without starting a mirror; a running job whose lease is still
// valid (another replica, flag on) is never touched. Afterwards the jobs are no longer live, so the orphan sweep and
// DELETE pin no longer see them as busy.
func TestPGSandboxFlagOffEndsInFlightJobs(t *testing.T) {
	db, jobs := pgFixture(t)
	ctx := context.Background()
	store := PGSandbox{DB: db}
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	cl := newFakeCluster()
	runner := func() *SandboxRunner {
		return &SandboxRunner{Store: jobs, Receipts: store, Lease: 10 * time.Second, CleanupOnly: true,
			Dead: store.DeadUncleaned, ClaimDead: jobs.ClaimDead,
			Build: func(context.Context, SandboxPayload) (*SandboxExecutor, error) {
				return &SandboxExecutor{Cluster: jobScopedCluster{cl}, Registry: &fakeReg{}, Receipts: store, Pins: store, Poll: time.Millisecond, PodWait: time.Second,
					SourceToken: func(context.Context, string) (string, error) {
						t.Error("a flag-off runner asked for a source token")
						return "", nil
					}}, nil
			}}
	}
	svc := &Service{DB: db, Jobs: jobs, Sandbox: store}
	expire := func(id string) {
		t.Helper()
		if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	live := func(id string) bool {
		t.Helper()
		ok, err := store.JobLiveByShort(ctx, jobShort(id))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// The worker pod an earlier acquisition (epoch e) of id left running: its Secrets are pod-owned.
	leftPod := func(id string, p SandboxPayload, e int64) string {
		name := sandbox.MirrorPodName(jobShort(id), e)
		cl.pods[name] = &sandbox.MirrorPod{Name: name, UID: "uid-" + name, Phase: "Running", Source: p.Source(), Dest: p.Destination(), JobShort: jobShort(id)}
		return name
	}
	deleted := func(name string) bool {
		for _, d := range cl.deleted {
			if d == name {
				return true
			}
		}
		return false
	}

	// 1. A queued mirror job (never claimed): ended, its receipt cleaned; nothing starts.
	base := sbxPayload()
	idQueued := sbxEnqueuePG(t, db, jobs, store, base, base.Key())
	if !live(idQueued) {
		t.Fatal("a queued job is not live")
	}
	if did, err := runner().RunOnce(ctx); !did || err != nil {
		t.Fatalf("queued: did=%v err=%v", did, err)
	}
	j, _ := jobs.Get(ctx, idQueued)
	rec, _ := store.Load(ctx, idQueued)
	if j.State != "dead" || j.LastError != "feature_disabled" || rec.Step != SbxFailed || rec.Code != "feature_disabled" || !rec.Cleaned {
		t.Fatalf("queued job %+v receipt %+v", j, rec)
	}
	if live(idQueued) {
		t.Fatal("the ended job is still live")
	}

	// 2. A running job whose lease EXPIRED (its runner is gone) mid-mirror, after it recorded the pin: its leftover
	// worker pod is deleted and the pin it replaced is restored (the prepull patch never landed).
	other := base
	other.Name = "other"
	before := SandboxPin{ID: other.Repo(), Template: "other", Registry: sbxReg, Digest: sbxOld, Ref: other.Repo() + ":20261101-aaaaaaa@" + sbxOld, MirrorJob: "older"}
	if err := store.SavePin(ctx, &before); err != nil {
		t.Fatal(err)
	}
	idExpired := sbxEnqueuePG(t, db, jobs, store, other, other.Key())
	j1, err := jobs.ClaimKinds(ctx, time.Minute, []string{durablejobs.TemplateUpdateSandbox})
	if err != nil || j1 == nil || j1.ID != idExpired {
		t.Fatalf("claim: %+v %v", j1, err)
	}
	podExpired := leftPod(idExpired, other, j1.Epoch)
	if err := jobs.Checkpoint(ctx, j1, func(tc context.Context) error {
		r, _ := store.Load(tc, idExpired)
		r.Step, r.Epoch, r.Pod, r.PodReady, r.PinRecorded, r.HadPin, r.PinBefore = SbxPrepull, j1.Epoch, podExpired, true, true, true, &before
		if err := store.Save(tc, r); err != nil {
			return err
		}
		return store.SavePin(tc, &SandboxPin{ID: other.Repo(), Template: "other", Registry: sbxReg, Digest: other.Digest, Ref: other.Ref(), MirrorJob: idExpired})
	}); err != nil {
		t.Fatal(err)
	}

	// 3. A running job with a LIVE lease (another replica with the flag on owns it): never touched.
	third := base
	third.Name = "third"
	idLive := sbxEnqueuePG(t, db, jobs, store, third, third.Key())
	jLive, err := jobs.ClaimKinds(ctx, time.Hour, []string{durablejobs.TemplateUpdateSandbox})
	if err != nil || jLive == nil || jLive.ID != idLive {
		t.Fatalf("claim live: %+v %v", jLive, err)
	}
	podLive := leftPod(idLive, third, jLive.Epoch)
	expire(idExpired)

	if did, err := runner().RunOnce(ctx); !did || err != nil {
		t.Fatalf("expired: did=%v err=%v", did, err)
	}
	j, _ = jobs.Get(ctx, idExpired)
	rec, _ = store.Load(ctx, idExpired)
	if j.State != "dead" || j.LastError != "feature_disabled" || rec.Step != SbxFailed || rec.Code != "feature_disabled" || !rec.Cleaned {
		t.Fatalf("expired job %+v receipt %+v", j, rec)
	}
	if !deleted(podExpired) {
		t.Fatalf("the expired job's worker pod (and its Secrets) survived: deleted=%v", cl.deleted)
	}
	if pin, _ := store.GetPin(ctx, other.Repo()); pin == nil || pin.MirrorJob != "older" || pin.Digest != sbxOld {
		t.Fatalf("the replaced pin was not restored: %+v", pin)
	}
	if live(idExpired) {
		t.Fatal("the ended job is still live")
	}
	// Idle now: the live-leased job is never claimed, however often the flag-off runner polls.
	for i := 0; i < 3; i++ {
		if did, err := runner().RunOnce(ctx); did || err != nil {
			t.Fatalf("round %d: did=%v err=%v", i, did, err)
		}
	}
	jl, _ := jobs.Get(ctx, idLive)
	if jl.State != "running" || jl.Epoch != jLive.Epoch || jl.Owner != jLive.Owner || deleted(podLive) || !live(idLive) {
		t.Fatalf("the live-leased job was touched: %+v deleted=%v", jl, cl.deleted)
	}
	if err := jobs.Checkpoint(ctx, jLive, nil); err != nil {
		t.Fatalf("the owning replica lost its lease: %v", err)
	}
	if len(cl.started) != 0 || cl.patches != 0 {
		t.Fatalf("a flag-off runner started a mirror (%d) or patched the prepull (%d)", len(cl.started), cl.patches)
	}

	// DELETE pin is no longer busy for the ended job's image (it was refused while the job was live).
	if _, err := svc.DeleteSandboxPin(ctx, other.Repo()); err != nil {
		t.Fatalf("delete pin after the job ended: %v", err)
	}
	if err := store.SavePin(ctx, &SandboxPin{ID: third.Repo(), Template: "third", Registry: sbxReg, Digest: sbxOld}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteSandboxPin(ctx, third.Repo()); !errors.Is(err, ErrSandboxBusy) {
		t.Fatalf("delete pin of the live-leased job's image: %v", err)
	}

	// 4. The live-leased job's runner dies on its LAST attempt: ClaimKinds (which also runs with the flag off) expires
	// it to dead; the dead-job pass then removes its worker pod. No job remains live.
	if _, err := db.Pool.Exec(ctx, `UPDATE durable_job SET attempts=max_attempts WHERE id=$1`, idLive); err != nil {
		t.Fatal(err)
	}
	expire(idLive)
	if did, err := runner().RunOnce(ctx); !did || err != nil {
		t.Fatalf("last attempt: did=%v err=%v", did, err)
	}
	jl, _ = jobs.Get(ctx, idLive)
	rec, _ = store.Load(ctx, idLive)
	if jl.State != "dead" || jl.LastError != "cleaned_up" || !rec.Cleaned || rec.Step != SbxFailed || !deleted(podLive) || live(idLive) {
		t.Fatalf("last attempt job %+v receipt %+v deleted=%v", jl, rec, cl.deleted)
	}
	if _, err := svc.DeleteSandboxPin(ctx, third.Repo()); err != nil {
		t.Fatalf("delete pin after the dead-job pass: %v", err)
	}
	if len(cl.started) != 0 || cl.patches != 0 || len(cl.pods) != 0 {
		t.Fatalf("started=%d patches=%d pods left %v", len(cl.started), cl.patches, cl.pods)
	}
}
