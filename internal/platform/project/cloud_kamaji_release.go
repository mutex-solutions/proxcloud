package project

// cloud_kamaji_release.go — releasing a deleted Managed Kubernetes cluster without waiting for a provider sync. The
// delete answers 202 while ArgoCD's cascade still runs, and CAPMOX deletes the workers with the very token the release
// revokes, so the delete queues a durable job that waits for the cascade (kamaji.Service.FinalizeCluster). The sync's
// sweep stays as the backstop.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

const (
	// kamajiReleaseLifetime is how long a job waits for one cascade. Past it the job is dead and the sweep is what
	// releases the cluster.
	kamajiReleaseLifetime = 24 * time.Hour
	kamajiReleaseMinWait  = 15 * time.Second
	kamajiReleaseMaxWait  = 5 * time.Minute
	// kamajiReleaseLease bounds one look. Its own figure, not the job worker's: a release may delete the disks of the
	// cluster's volumes, which takes longer than a database transaction.
	kamajiReleaseLease = 5 * time.Minute
)

type kamajiReleasePayload struct {
	ServiceID string `json:"serviceId"` // the Kubernetes provider
	ProjectID string `json:"projectId"`
	ClusterID string `json:"clusterId"`
}

// SetKamajiReleaseJobs wires the durable job store a cluster delete queues its release on (nil: the sweep alone).
func (h *Handler) SetKamajiReleaseJobs(q interface {
	Enqueue(ctx context.Context, kind, key string, payload []byte, maxAttempts int) (string, error)
}) {
	h.kamajiReleaseJobs = q
}

// kamajiQueueRelease runs after the cluster's Application delete was accepted. A failure is logged and fails nothing:
// the Application is gone already, and the sweep releases the cluster at a provider sync.
func (h *Handler) kamajiQueueRelease(ctx context.Context, serviceID, projectID, clusterID string) {
	if h.kamajiReleaseJobs == nil {
		return
	}
	raw, _ := json.Marshal(kamajiReleasePayload{ServiceID: serviceID, ProjectID: projectID, ClusterID: clusterID})
	// 20 attempts bound the crashes of an acquisition; waiting spends none (Defer).
	if _, err := h.kamajiReleaseJobs.Enqueue(ctx, durablejobs.KamajiRelease, serviceID+"/"+clusterID, raw, 20); err != nil {
		slog.Warn("kamaji: release not queued, the provider sync's sweep releases the cluster", "cluster", clusterID, "err", err)
	}
}

// KamajiFinalizeCluster is the release job's step: done when nothing of the cluster is left to release. A provider
// that no longer exists has no management cluster to ask, so there is nothing this job can do.
func (h *Handler) KamajiFinalizeCluster(ctx context.Context, serviceID, projectID, clusterID string) (bool, error) {
	es, err := h.esSvc.Get(ctx, serviceID)
	if errors.Is(err, externalservice.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if h.kamajiFor == nil {
		return false, errors.New("managed kubernetes is not configured")
	}
	ks, err := h.kamajiFor(es)
	if err != nil {
		return false, err
	}
	return ks.FinalizeCluster(ctx, projectID, clusterID)
}

// KamajiReleaseJobs is the subset of *durablejobs.Store the runner uses.
type KamajiReleaseJobs interface {
	ClaimKinds(ctx context.Context, ttl time.Duration, kinds []string) (*durablejobs.Job, error)
	Complete(ctx context.Context, j *durablejobs.Job, fn func(context.Context) error) error
	Defer(ctx context.Context, j *durablejobs.Job, delay time.Duration, code string) error
	Fail(ctx context.Context, j *durablejobs.Job, delay time.Duration, permanent bool, code string) error
}

// KamajiReleaseRunner claims KamajiRelease jobs one at a time. It runs with or without the job scheduler.
type KamajiReleaseRunner struct {
	Jobs KamajiReleaseJobs
	// Finalize is Handler.KamajiFinalizeCluster.
	Finalize func(ctx context.Context, serviceID, projectID, clusterID string) (done bool, err error)
	Poll     time.Duration
	Log      *slog.Logger
}

// Run polls until ctx ends.
func (r *KamajiReleaseRunner) Run(ctx context.Context) {
	if r.Poll <= 0 {
		r.Poll = 5 * time.Second
	}
	for ctx.Err() == nil {
		did, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.Log.Warn("kamaji release runner", "err", err)
		}
		if did {
			continue
		}
		t := time.NewTimer(r.Poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// RunOnce claims at most one job and looks at its cluster once; did reports whether one was claimed.
//
// One look is a few short reads and, when the cascade is over, the release. It is bounded to three quarters of the
// lease and not renewed. Should the lease be lost all the same, a second acquisition repeats steps that are all
// idempotent (a token already deleted counts as revoked), which is why this kind keeps no receipts.
func (r *KamajiReleaseRunner) RunOnce(ctx context.Context) (bool, error) {
	j, err := r.Jobs.ClaimKinds(ctx, kamajiReleaseLease, []string{durablejobs.KamajiRelease})
	if err != nil || j == nil {
		return false, err
	}
	var p kamajiReleasePayload
	if json.Unmarshal(j.Payload, &p) != nil || p.ServiceID == "" || p.ProjectID == "" || p.ClusterID == "" {
		return true, r.Jobs.Fail(ctx, j, 0, true, "invalid_payload")
	}
	work, cancel := context.WithTimeout(ctx, kamajiReleaseLease*3/4)
	done, ferr := r.Finalize(work, p.ServiceID, p.ProjectID, p.ClusterID)
	cancel()
	if ctx.Err() != nil {
		return true, nil // stopping: the lease expires and the next process looks again
	}
	// The job's own writes are detached from a look that used up its time.
	ack := context.WithoutCancel(ctx)
	if ferr == nil && done {
		r.Log.Info("kamaji: deleted cluster released", "cluster", p.ClusterID, "project", p.ProjectID)
		return true, r.Jobs.Complete(ack, j, nil)
	}
	age := time.Since(j.CreatedAt)
	if ferr != nil {
		// Never the error text in the job row: it may quote a provider's answer.
		r.Log.Warn("kamaji: release of a deleted cluster, will retry", "cluster", p.ClusterID, "project", p.ProjectID, "age", age.Round(time.Second), "err", ferr)
	}
	if age >= kamajiReleaseLifetime {
		r.Log.Warn("kamaji: release job gave up, the provider sync's sweep releases the cluster", "cluster", p.ClusterID, "project", p.ProjectID)
		return true, r.Jobs.Fail(ack, j, 0, true, "cascade_not_finished")
	}
	code := "cascade_running"
	if ferr != nil {
		code = "release_failed"
	}
	return true, r.Jobs.Defer(ack, j, kamajiReleaseWait(age), code)
}

// kamajiReleaseWait backs off with the job's age: a quarter of it, between 15 seconds and 5 minutes. A cascade that
// takes its usual minute is seen over within seconds; one that is stuck is asked every 5 minutes.
func kamajiReleaseWait(age time.Duration) time.Duration {
	return min(max(age/4, kamajiReleaseMinWait), kamajiReleaseMaxWait)
}
