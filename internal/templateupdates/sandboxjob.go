package templateupdates

// sandboxjob.go — one acquisition of a sandbox mirror/rollback job. Every cluster mutation (worker pod, its Secrets,
// a pod delete, the prepull patch) is immediately preceded by a successful fence check; every step is recorded in the
// receipt through the fence, so a new acquisition resumes from the last committed step and a superseded one performs
// no further effect.
//
//	mirror:   queued → mirroring (intent: pod name) → mirrored (worker result valid) → verified (portal read: same
//	          digest) → prepull (intent: pin + previous images recorded) → pinned (DaemonSets run the ref) → succeeded
//	rollback: queued → verified (target still in the registry, by digest) → prepull → pinned → succeeded

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

// SandboxCluster is the subset of *sandbox.Service the executor uses (a fake in tests).
type SandboxCluster interface {
	StartMirror(ctx context.Context, req sandbox.MirrorRequest) (string, error)
	GetMirrorPod(ctx context.Context, jobShort, name string) (*sandbox.MirrorPod, error)
	MirrorResult(ctx context.Context, p *sandbox.MirrorPod) (*sandboximport.MirrorResult, error)
	DeleteMirrorPod(ctx context.Context, name, uid string) error
	ListMirrorPods(ctx context.Context, jobShort string) ([]sandbox.MirrorPod, error)
	PrepullImages(ctx context.Context, repo string) ([]sandbox.PrepullImage, error)
	SetPrepullImage(ctx context.Context, ref string) ([]sandbox.PrepullImage, bool, error)
	RestorePrepullImages(ctx context.Context, current string, restore []sandbox.PrepullImage) (int, error)
}

// DigestResolver reads a manifest from the sandbox registry with the mirror account (read-only use) and returns the
// sha256 of its bytes.
type DigestResolver interface {
	Resolve(ctx context.Context, repoPath, reference string) (string, error)
}

// ErrRegistryNotFound is a manifest the registry does not have.
var ErrRegistryNotFound = errors.New("registry: manifest not found")

// ErrRegistryRefused is a registry refusal of the mirror account (401/403).
var ErrRegistryRefused = errors.New("registry: the mirror account was refused")

// ErrMirrorWorkerFailed (fixed code mirror_worker_failed) is a worker pod that ended Failed — e.g. its short-lived
// anonymous GHCR token expired during a slow copy. Retryable: the next attempt mints a fresh token for a new pod.
var ErrMirrorWorkerFailed = errors.New("mirror_worker_failed: the mirror worker failed; the next attempt uses a fresh source token")

const mirrorWorkerFailedWarn = "mirror_worker_failed: a mirror worker failed (an expired source token or a transient error); retried with a fresh anonymous GHCR token"

// SandboxExecutor runs one acquisition.
type SandboxExecutor struct {
	Cluster  SandboxCluster
	Registry DigestResolver
	Receipts SandboxReceipts
	Pins     SandboxPins
	// SourceToken mints, at run time, an ANONYMOUS short-lived GHCR registry token with pull scope on the one public
	// source package (Client.PullToken; mirror only). The PAT is used for discovery only and never takes part in it nor
	// leaves the portal; the token is checked (CheckPullToken) before it is written to the worker Secret, never stored.
	SourceToken func(ctx context.Context, pkg string) (string, error)
	// Poll / PodWait bound the wait for the worker pod (defaults 5s / sandbox.MirrorTimeout+5m); PendingLimit how
	// long it may stay Pending before it is replaced (default sandbox.MirrorPendingLimit).
	Poll, PodWait, PendingLimit time.Duration
}

func (x *SandboxExecutor) checkpoint(ctx context.Context, f Fence, r *SandboxReceipt, step Step, mut func(context.Context, *SandboxReceipt) error) error {
	next := *r
	next.Step, next.Epoch, next.UpdatedAt = step, f.Epoch(), time.Now().UTC()
	err := f.Checkpoint(ctx, func(tc context.Context) error {
		if mut != nil {
			if err := mut(tc, &next); err != nil {
				return err
			}
		}
		return x.Receipts.Save(tc, &next)
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStopped, err)
	}
	*r = next
	return nil
}

func (x *SandboxExecutor) guard(ctx context.Context, f Fence) error {
	if err := f.Checkpoint(ctx, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrStopped, err)
	}
	return nil
}

// Run executes the job from its last receipt: nil = pinned (the runner marks it succeeded), *Permanent = terminal,
// ErrStopped (wrapped) = fence lost, anything else = retry later.
func (x *SandboxExecutor) Run(ctx context.Context, jobID string, p SandboxPayload, f Fence) error {
	if err := p.Validate(); err != nil {
		return permanent("invalid_payload", "the job payload failed validation")
	}
	r, err := x.Receipts.Load(ctx, jobID)
	if err != nil {
		return err
	}
	if r == nil || r.Payload != p {
		return permanent("receipt_mismatch", "the job receipt is missing or belongs to another payload")
	}
	if x.Poll <= 0 {
		x.Poll = 5 * time.Second
	}
	if x.PodWait <= 0 {
		x.PodWait = sandbox.MirrorTimeout + 5*time.Minute
	}
	switch r.Step {
	case SbxPinned, SbxSucceeded:
		return nil
	case SbxFailed:
		return permanent(r.Code, r.Message)
	}
	if p.Action == SandboxMirror && (r.Step == SbxQueued || r.Step == SbxMirroring) {
		if err := x.mirror(ctx, jobID, p, r, f); err != nil {
			return err
		}
	}
	if (p.Action == SandboxMirror && r.Step == SbxMirrored) || (p.Action == SandboxRollback && r.Step == SbxQueued) {
		if err := x.verify(ctx, p, r, f); err != nil {
			return err
		}
	}
	if r.Step == SbxVerified || r.Step == SbxPrepull {
		if err := x.prepull(ctx, jobID, p, r, f); err != nil {
			return err
		}
	}
	if r.Step != SbxPinned {
		return fmt.Errorf("sandbox update: unexpected step %s", r.Step)
	}
	// Best-effort, fenced: no worker pod of this job (any epoch) outlives it; their Secrets go with them.
	if warns := x.sweepPods(ctx, f, jobID); len(warns) > 0 && !stopped(warns) {
		_ = x.checkpoint(ctx, f, r, r.Step, func(_ context.Context, n *SandboxReceipt) error {
			n.Warnings = append(n.Warnings, warns...)
			return nil
		})
	}
	return nil
}

func (x *SandboxExecutor) sweepPods(ctx context.Context, f Fence, jobID string) []string {
	pods, err := x.Cluster.ListMirrorPods(ctx, jobShort(jobID))
	if err != nil {
		return []string{"could not list this job's mirror worker pods"}
	}
	var warns []string
	for _, pod := range pods {
		if x.guard(ctx, f) != nil {
			return append(warns, stoppedWarn)
		}
		if err := x.Cluster.DeleteMirrorPod(ctx, pod.Name, pod.UID); err != nil {
			warns = append(warns, "mirror worker pod "+pod.Name+" could not be deleted")
		}
	}
	return warns
}

// mirror runs (or resumes) the worker pod of this acquisition until it reports a valid result.
func (x *SandboxExecutor) mirror(ctx context.Context, jobID string, p SandboxPayload, r *SandboxReceipt, f Fence) error {
	short := jobShort(jobID)
	name := ""
	// An earlier acquisition's pod: a result it already has is used; one still running whose Secrets were written
	// (PodReady) is waited for; a failed one, or one whose Secrets may be missing, is removed and the copy redone
	// under this epoch's own pod.
	if r.Step == SbxMirroring && r.Pod != "" {
		pod, err := x.Cluster.GetMirrorPod(ctx, short, r.Pod)
		if err != nil {
			return err
		}
		if pod != nil {
			done, err := x.settlePod(ctx, f, p, r, pod)
			if done && err == nil {
				return nil
			}
			if errors.Is(err, ErrStopped) || isPermanent(err) {
				return err
			}
			switch {
			case !done && r.PodReady:
				name = pod.Name
			case !done:
				x.dropPod(ctx, f, r, pod) // its credential Secrets may never have been written
			}
		}
	}
	if name == "" {
		tok, err := x.SourceToken(ctx, SandboxPackage(p.Name))
		if err == nil {
			// Defense in depth (the minting side checks it against the PAT): never write a GitHub token, an empty or
			// malformed value into the worker Secret.
			err = CheckPullToken(tok, "")
		}
		if err != nil {
			var pe *Permanent
			switch {
			case errors.As(err, &pe) && (pe.Code == CodeTokenUnscoped || pe.Code == CodePackageNotPublic):
				return permanent(pe.Code, pe.Message)
			case errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrNoToken):
				return permanent("github_not_configured", "the GitHub integration has no token")
			case isPermanent(err):
				return permanent(CodeTokenUnscoped, "the GHCR source token was refused")
			}
			return errors.New("a GHCR registry token could not be obtained")
		}
		name = sandbox.MirrorPodName(short, f.Epoch())
		if err := x.checkpoint(ctx, f, r, SbxMirroring, func(_ context.Context, n *SandboxReceipt) error {
			n.Pod, n.PodReady = name, false
			return nil
		}); err != nil {
			return err
		}
		if err := x.guard(ctx, f); err != nil {
			return err
		}
		_, err = x.Cluster.StartMirror(ctx, sandbox.MirrorRequest{JobShort: short, Epoch: f.Epoch(), Source: p.Source(), Destination: p.Destination(), SourceToken: tok})
		if errors.Is(err, sandbox.ErrMirrorUnavailable) {
			return permanent("mirror_unavailable", "the mirror prerequisites are missing (worker image, template registry or the registry mirror account)")
		}
		if err != nil {
			// The pod may exist without its Secrets: remove it now rather than leave it pending.
			if pod, gerr := x.Cluster.GetMirrorPod(ctx, short, name); gerr == nil && pod != nil {
				x.dropPod(ctx, f, r, pod)
			}
			return errors.New("the mirror worker could not be started")
		}
		if err := x.checkpoint(ctx, f, r, SbxMirroring, func(_ context.Context, n *SandboxReceipt) error { n.PodReady = true; return nil }); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(x.PodWait)
	pendingSince := time.Time{}
	for {
		pod, err := x.Cluster.GetMirrorPod(ctx, short, name)
		if err == nil && pod != nil {
			done, err := x.settlePod(ctx, f, p, r, pod)
			if done || err != nil {
				return err
			}
			// A pod that never leaves Pending (image pull, scheduling, missing Secrets) is replaced, not waited out.
			if pod.Phase == "Pending" || pod.Phase == "" {
				if pendingSince.IsZero() {
					pendingSince = time.Now()
				}
				if time.Since(pendingSince) > x.pendingLimit() {
					x.dropPod(ctx, f, r, pod)
					return errors.New("the mirror worker stayed pending; it was removed and will be started again")
				}
			} else {
				pendingSince = time.Time{}
			}
		}
		if time.Now().After(deadline) {
			return errors.New("the mirror worker did not finish in time")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrStopped, ctx.Err())
		case <-time.After(x.Poll):
		}
	}
}

func (x *SandboxExecutor) pendingLimit() time.Duration {
	if x.PendingLimit > 0 {
		return x.PendingLimit
	}
	return sandbox.MirrorPendingLimit
}

// settlePod handles a worker pod in a terminal phase: Succeeded with a valid result → mirrored (and the pod, with
// its credential Secrets, is deleted at once); Failed → deleted, retried. done=false: still running.
func (x *SandboxExecutor) settlePod(ctx context.Context, f Fence, p SandboxPayload, r *SandboxReceipt, pod *sandbox.MirrorPod) (bool, error) {
	if pod.Source != p.Source() || pod.Dest != p.Destination() {
		return true, permanent("ownership_lost", "the mirror worker pod does not carry this job's source and destination")
	}
	switch pod.Phase {
	case "Succeeded":
		res, err := x.Cluster.MirrorResult(ctx, pod)
		if err != nil {
			return true, permanent("mirror_result_invalid", "the mirror worker's result does not prove the same digest")
		}
		if err := x.checkpoint(ctx, f, r, SbxMirrored, func(_ context.Context, n *SandboxReceipt) error { n.SizeBytes = res.SizeBytes; return nil }); err != nil {
			return true, err
		}
		x.dropPod(ctx, f, r, pod)
		return true, nil
	case "Failed":
		// Retryable, never permanent: the anonymous source token cannot be refreshed inside the worker, so a copy that
		// outlives it (or any transient failure) fails the pod; the next attempt mints a fresh token for a new pod.
		x.dropPod(ctx, f, r, pod)
		if err := x.checkpoint(ctx, f, r, r.Step, func(_ context.Context, n *SandboxReceipt) error {
			n.Warnings = appendOnce(n.Warnings, mirrorWorkerFailedWarn)
			return nil
		}); err != nil {
			return true, err
		}
		return true, ErrMirrorWorkerFailed
	}
	return false, nil
}

// dropPod deletes one observed worker (UID precondition) right after a fence check; its Secrets go with it. A
// failed delete is recorded in the receipt (fenced): the job's sweep and the orphan sweep retry it.
func (x *SandboxExecutor) dropPod(ctx context.Context, f Fence, r *SandboxReceipt, pod *sandbox.MirrorPod) {
	if x.guard(ctx, f) != nil {
		return
	}
	if err := x.Cluster.DeleteMirrorPod(ctx, pod.Name, pod.UID); err != nil {
		w := "the mirror worker pod " + pod.Name + " could not be deleted yet"
		_ = x.checkpoint(ctx, f, r, r.Step, func(_ context.Context, n *SandboxReceipt) error {
			n.Warnings = append(n.Warnings, w)
			return nil
		})
	}
}

// verify reads the destination back with the mirror account: the mirrored tag (or, for a rollback, the target
// digest) must resolve to exactly the payload digest.
func (x *SandboxExecutor) verify(ctx context.Context, p SandboxPayload, r *SandboxReceipt, f Fence) error {
	ref := p.Tag
	if p.Action == SandboxRollback {
		ref = p.Digest
	}
	got, err := x.Registry.Resolve(ctx, "library/sandbox-"+p.Name, ref)
	switch {
	case errors.Is(err, ErrRegistryNotFound) && p.Action == SandboxRollback:
		return permanent("rollback_target_missing", "the previous image is no longer in the sandbox registry")
	case errors.Is(err, ErrRegistryNotFound):
		return permanent("digest_mismatch", "the mirrored tag does not resolve in the sandbox registry")
	case errors.Is(err, ErrRegistryRefused):
		return permanent("registry_refused", "the sandbox registry refused the mirror account (read)")
	case err != nil:
		return errors.New("the sandbox registry could not be read")
	case got != p.Digest:
		return permanent("digest_mismatch", "the sandbox registry resolves the image to another digest")
	}
	return x.checkpoint(ctx, f, r, SbxVerified, func(_ context.Context, n *SandboxReceipt) error { n.VerifiedDigest = got; return nil })
}

// prepull records the pin and the images it replaces (one fenced transaction), then patches the DaemonSets. Each
// DaemonSet is its own guarded patch: when one lands and the other fails, the landed containers are recorded and the
// attempt retries (the patch is idempotent: what already runs the ref is left as is); a job that ends without both
// landed rolls the landed ones back (Cleanup), so the two DaemonSets never stay on different digests.
func (x *SandboxExecutor) prepull(ctx context.Context, jobID string, p SandboxPayload, r *SandboxReceipt, f Fence) error {
	if r.Step == SbxVerified {
		cur, err := x.Cluster.PrepullImages(ctx, p.Repo())
		if err != nil {
			return err
		}
		previous := make([]PrepullImageRecord, 0, len(cur))
		prevRef := ""
		for _, c := range cur {
			previous = append(previous, PrepullImageRecord{DaemonSet: c.DaemonSet, Container: c.Container, Image: c.Image})
			if prevRef == "" && c.Image != p.Ref() {
				prevRef = c.Image
			}
		}
		err = x.checkpoint(ctx, f, r, SbxPrepull, func(tc context.Context, n *SandboxReceipt) error {
			before, err := x.Pins.GetPin(tc, p.Repo())
			if err != nil {
				return err
			}
			n.Previous, n.PinBefore, n.HadPin, n.PinRecorded = previous, before, before != nil, true
			if prevRef == "" && before != nil && before.Digest != p.Digest {
				prevRef = before.Ref
			}
			return x.Pins.SavePin(tc, &SandboxPin{ID: p.Repo(), Template: p.Name, Registry: p.Registry, Tag: p.Tag, Digest: p.Digest,
				Ref: p.Ref(), Action: p.Action, Previous: prevRef, ServiceID: p.ServiceID, Actor: r.RequestedBy, At: time.Now().UTC(), MirrorJob: jobID})
		})
		if err != nil {
			return err
		}
	}
	if err := x.guard(ctx, f); err != nil {
		return err
	}
	if _, _, err := x.Cluster.SetPrepullImage(ctx, p.Ref()); err != nil {
		// Record what landed (one DaemonSet may already run the ref) for the retry and for Cleanup's rollback.
		if cur, cerr := x.Cluster.PrepullImages(ctx, p.Repo()); cerr == nil {
			var landed []string
			for _, c := range cur {
				if c.Image == p.Ref() {
					landed = append(landed, c.DaemonSet+"/"+c.Container)
				}
			}
			if len(landed) > 0 {
				_ = x.checkpoint(ctx, f, r, SbxPrepull, func(_ context.Context, n *SandboxReceipt) error {
					n.Landed = landed
					n.Warnings = appendOnce(n.Warnings, "the prepull switch landed on "+strings.Join(landed, ", ")+" only; retrying the rest")
					return nil
				})
			}
		}
		return errors.New("the prepull DaemonSet could not be patched (retrying)")
	}
	return x.checkpoint(ctx, f, r, SbxPinned, func(_ context.Context, n *SandboxReceipt) error { n.Landed = nil; return nil })
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// Cleanup runs after a terminal failure (or for a dead job): it deletes this job's worker pods (their credential
// Secrets go with them); when the prepull switch did not complete, it points every prepull init container still on
// this job's ref back at the image recorded before (so the DaemonSets never stay mixed) and restores the pin record
// this job replaced. Each effect is preceded by a fence check.
func (x *SandboxExecutor) Cleanup(ctx context.Context, f Fence, jobID string, p SandboxPayload, r *SandboxReceipt) []string {
	warns := x.sweepPods(ctx, f, jobID)
	if stopped(warns) {
		return warns
	}
	if r == nil || !r.PinRecorded || r.Step == SbxPinned || r.Step == SbxSucceeded {
		return warns
	}
	if len(r.Previous) > 0 {
		restore := make([]sandbox.PrepullImage, 0, len(r.Previous))
		for _, pr := range r.Previous {
			restore = append(restore, sandbox.PrepullImage{DaemonSet: pr.DaemonSet, Container: pr.Container, Image: pr.Image})
		}
		if x.guard(ctx, f) != nil {
			return append(warns, stoppedWarn)
		}
		if n, err := x.Cluster.RestorePrepullImages(ctx, p.Ref(), restore); err != nil {
			return append(warns, "the partial prepull switch could not be rolled back; re-run the update or roll back from the page")
		} else if n > 0 {
			warns = append(warns, fmt.Sprintf("rolled %d prepull init container(s) back to their previous image", n))
		}
	}
	cur, err := x.Cluster.PrepullImages(ctx, p.Repo())
	if err != nil {
		return append(warns, "could not read the prepull DaemonSets to settle the pin record")
	}
	for _, c := range cur {
		if c.Image == p.Ref() {
			// Still on the new ref (an added p<N>, or the restore did not reach it): the pin stays true to the cluster.
			return append(warns, "a prepull init container still runs this job's image; the pin record is kept")
		}
	}
	err = f.Checkpoint(ctx, func(tc context.Context) error {
		pin, err := x.Pins.GetPin(tc, p.Repo())
		if err != nil || pin == nil || pin.MirrorJob != jobID {
			return err // another job's pin since: leave it
		}
		if r.HadPin && r.PinBefore != nil {
			return x.Pins.SavePin(tc, r.PinBefore)
		}
		return x.Pins.DeletePin(tc, p.Repo())
	})
	if err != nil {
		return append(warns, stoppedWarn)
	}
	return warns
}

// ── the portal's read of the sandbox registry ─────────────────────────────────────────────────────────────────

// RegistryReader resolves manifests on the sandbox registry with basic auth (the mirror account), trusting the
// registry's own CA.
type RegistryReader struct {
	base, user, pass string
	http             *http.Client
}

// NewRegistryReader builds a reader for https://host (scheme "http" only in tests).
func NewRegistryReader(host, caPEM, user, pass, scheme string) *RegistryReader {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if caPEM != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(caPEM))
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	if scheme == "" {
		scheme = "https"
	}
	return &RegistryReader{base: scheme + "://" + host, user: user, pass: pass,
		http: &http.Client{Timeout: 30 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("registry redirects are not followed") }}}
}

func (r *RegistryReader) String() string {
	return "templateupdates.RegistryReader{password:[REDACTED]}"
}

// Resolve GETs the manifest and returns the sha256 of its bytes; a Docker-Content-Digest that disagrees is refused.
func (r *RegistryReader) Resolve(ctx context.Context, repoPath, reference string) (string, error) {
	if !strings.HasPrefix(repoPath, "library/sandbox-") || strings.ContainsAny(reference, "/?#") || strings.Contains(repoPath, "..") {
		return "", errors.New("registry: refusing an unexpected reference")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+"/v2/"+repoPath+"/manifests/"+reference, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(r.user, r.pass)
	req.Header.Set("Accept", strings.Join([]string{MediaOCIManifest, MediaOCIIndex, MediaDockerManifest, MediaDockerList}, ", "))
	res, err := r.http.Do(req)
	if err != nil {
		return "", errors.New("registry: request failed")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxManifestBytes+1))
	if err != nil {
		return "", errors.New("registry: read failed")
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return "", ErrRegistryNotFound
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return "", ErrRegistryRefused
	case res.StatusCode != http.StatusOK:
		return "", fmt.Errorf("registry: %d", res.StatusCode)
	case len(body) > maxManifestBytes:
		return "", errors.New("registry: manifest too large")
	}
	sum := sha256.Sum256(body)
	d := "sha256:" + hex.EncodeToString(sum[:])
	if h := res.Header.Get("Docker-Content-Digest"); h != "" && h != d {
		return "", errors.New("registry: the content digest header disagrees with the manifest bytes")
	}
	return d, nil
}

func isPermanent(err error) bool {
	var p *Permanent
	return errors.As(err, &p)
}
