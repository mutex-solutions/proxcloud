package templateupdates

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
)

// N4: a job's cleanup needs the provider's cluster only. Build refuses the job permanently once the provider lost its
// mirror account (cmd/api: mirror_unavailable); BuildCleanup (cluster + store: no mirror account, registry reader or
// GitHub token) still deletes the job's worker pods (their pod-owned Secrets go with them), rolls the partial prepull
// switch back and restores the pin, on the dead-job pass and on the terminal path alike.
func TestSandboxCleanupWithoutTheMirrorAccount(t *testing.T) {
	p := sbxPayload()
	old := sbxReg + "/library/sandbox-base:20261101-aaaaaaa@" + sbxOld
	before := SandboxPin{ID: p.Repo(), Template: "base", Registry: sbxReg, Digest: sbxOld, Ref: old, MirrorJob: "older"}
	pod := sandbox.MirrorPodName(jobShort(sbxJob), 1)
	// A job that ended mid-switch: its worker pod is left, its pin recorded, one DaemonSet already on its ref.
	leftover := func(t *testing.T) (*SandboxRunner, *sbxJobs, *fakeCluster, *memSandbox) {
		t.Helper()
		r, jobs, cl, _, mem := sbxFixture(t, p)
		cl.pods[pod] = &sandbox.MirrorPod{Name: pod, UID: "u1", Phase: "Running", Source: p.Source(), Dest: p.Destination()}
		cl.prepull["template-prepull/p1"], cl.prepull["template-prepull-gpu/p0"] = p.Ref(), old
		mem.pins[p.Repo()] = SandboxPin{ID: p.Repo(), Template: "base", Registry: sbxReg, Digest: p.Digest, Ref: p.Ref(), Previous: old, MirrorJob: sbxJob}
		rec := mem.recs[sbxJob]
		rec.Step, rec.Pod, rec.PodReady, rec.VerifiedDigest = SbxPrepull, pod, true, p.Digest
		rec.PinRecorded, rec.HadPin, rec.PinBefore, rec.Landed = true, true, &before, []string{"template-prepull/p1"}
		rec.Previous = []PrepullImageRecord{{DaemonSet: "template-prepull", Container: "p1", Image: old}, {DaemonSet: "template-prepull-gpu", Container: "p0", Image: old}}
		mem.recs[sbxJob] = rec
		r.Build = func(context.Context, SandboxPayload) (*SandboxExecutor, error) {
			return nil, &Permanent{Code: "mirror_unavailable", Message: "the sandbox provider has no registry mirror account (secret.registryMirror)"}
		}
		r.BuildCleanup = func(_ context.Context, q SandboxPayload) (*SandboxExecutor, error) {
			if q != p {
				t.Errorf("cleanup built for %+v", q)
			}
			return &SandboxExecutor{Cluster: cl, Receipts: mem, Pins: mem}, nil
		}
		return r, jobs, cl, mem
	}
	cleaned := func(t *testing.T, cl *fakeCluster, mem *memSandbox) {
		t.Helper()
		if len(cl.pods) != 0 || len(cl.deleted) != 1 || cl.deleted[0] != pod {
			t.Fatalf("worker pods (and their pod-owned Secrets) left: pods=%v deleted=%v", cl.pods, cl.deleted)
		}
		if cl.prepull["template-prepull/p1"] != old || cl.prepull["template-prepull-gpu/p0"] != old {
			t.Fatalf("the partial prepull switch was not rolled back: %v", cl.prepull)
		}
		if got := mem.pins[p.Repo()]; got != before {
			t.Fatalf("the pin was not restored: %+v", got)
		}
		if got := mem.recs[sbxJob]; got.Step != SbxFailed || !got.Cleaned {
			t.Fatalf("receipt %+v", got)
		}
	}
	// No job is claimable to run; the dead-job pass re-acquires the dead job, fenced.
	claimDead := func(r *SandboxRunner, jobs *sbxJobs) {
		jobs.claimed = true
		r.Dead = func(context.Context) ([]string, error) { return []string{sbxJob}, nil }
		r.ClaimDead = func(context.Context, string, time.Duration) (*durablejobs.Job, error) {
			j := *jobs.job
			j.Epoch, j.Attempts = 2, j.MaxAttempts
			return &j, nil
		}
	}

	t.Run("dead-job pass", func(t *testing.T) {
		r, jobs, cl, mem := leftover(t)
		claimDead(r, jobs)
		if did, err := r.RunOnce(context.Background()); !did || err != nil {
			t.Fatalf("cleanup pass: %v %v", did, err)
		}
		cleaned(t, cl, mem)
		if jobs.failCode != "cleaned_up" || !jobs.permanent {
			t.Fatalf("job %s permanent=%v", jobs.failCode, jobs.permanent)
		}
	})
	t.Run("terminal", func(t *testing.T) {
		r, jobs, cl, mem := leftover(t)
		if did, err := r.RunOnce(context.Background()); !did || err != nil {
			t.Fatalf("run: %v %v", did, err)
		}
		cleaned(t, cl, mem)
		if got := mem.recs[sbxJob]; got.Code != "mirror_unavailable" || jobs.failCode != "mirror_unavailable" || !jobs.permanent {
			t.Fatalf("receipt %+v job %s", got, jobs.failCode)
		}
	})
	t.Run("provider gone for good", func(t *testing.T) {
		r, jobs, cl, mem := leftover(t)
		claimDead(r, jobs)
		r.BuildCleanup = func(context.Context, SandboxPayload) (*SandboxExecutor, error) {
			return nil, &Permanent{Code: "provider_missing", Message: "the target sandbox provider no longer exists"}
		}
		if did, err := r.RunOnce(context.Background()); !did || err != nil {
			t.Fatalf("cleanup pass: %v %v", did, err)
		}
		got := mem.recs[sbxJob]
		if !got.Cleaned || len(got.Warnings) == 0 || !strings.Contains(got.Warnings[len(got.Warnings)-1], "provider_missing") || jobs.failCode != "cleaned_up" || len(cl.deleted) != 0 {
			t.Fatalf("receipt %+v job %s deleted %v", got, jobs.failCode, cl.deleted)
		}
	})
	t.Run("cluster not reachable now", func(t *testing.T) {
		r, jobs, cl, mem := leftover(t)
		claimDead(r, jobs)
		r.BuildCleanup = func(context.Context, SandboxPayload) (*SandboxExecutor, error) {
			return nil, errors.New("kubeconfig unusable")
		}
		if did, err := r.RunOnce(context.Background()); did || err != nil {
			t.Fatalf("cleanup pass: %v %v", did, err)
		}
		if mem.recs[sbxJob].Cleaned || jobs.failCode != "cleanup_pending" || len(cl.deleted) != 0 {
			t.Fatalf("receipt %+v job %s", mem.recs[sbxJob], jobs.failCode)
		}
		// The terminal path leaves such a job uncleaned: the dead-job pass retries it.
		r, jobs, _, mem = leftover(t)
		r.BuildCleanup = func(context.Context, SandboxPayload) (*SandboxExecutor, error) {
			return nil, errors.New("kubeconfig unusable")
		}
		if did, err := r.RunOnce(context.Background()); !did || err != nil {
			t.Fatalf("run: %v %v", did, err)
		}
		if got := mem.recs[sbxJob]; got.Step != SbxFailed || got.Cleaned || jobs.failCode != "mirror_unavailable" {
			t.Fatalf("receipt %+v job %s", got, jobs.failCode)
		}
	})
}
