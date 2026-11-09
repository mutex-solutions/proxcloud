package project

// e2b_snapshots.go — pause, resume, snapshots and fork of a sandbox's filesystem (SB7, docs/managed-sandbox.md §8).
// Ported from the earlier implementation (EE).
//
// A snapshot is a template (as in E2B: Sandbox.create(snapshotID), DELETE /templates/{id}):
//  1. `sandbox-init -snapshot` in the sandbox streams a gzip tar of everything changed since boot; the portal streams
//     it into the provider's build bucket (one PUT up to 16 MiB, else multipart, UNSIGNED-PAYLOAD);
//  2. a pod in sbx-build fetches it with a presigned GET and `crane append`s it as one layer onto the sandbox's image,
//     pushing <templateRegistry>/<project>/<template>:<build> with the project's build account.
//
// Pause = snapshot + save the sandbox's CR (tokens, env, metadata, network), answered once the capture is stored; the
// sandbox shows as paused but runs (and bills) until its image is pushed, then it is deleted (finishPause). Resume (POST
// /sandboxes/{id}/resume, connect to a paused sandbox, or the wake endpoint for autoResume) recreates it under the
// same id from the snapshot image. Processes, memory and open connections are not kept (E2B's mode "filesystem"); a
// request for memory (memory: true, autoPauseMemory: true) is refused with 400, never served as a filesystem pause.
// Fork = snapshot + up to 10 clones with new ids and new tokens. autoPause pauses at the timeout (job
// sandboxAutoPause). A stored snapshot is billed per GB-hour (SANDBOX_SNAPSHOT) until it is resumed or deleted.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	snapshotWait = 15 * time.Minute // how long a background assembly is followed
	maxForkCount = 10
	// autoPauseParallel: pauses the job runs at once (each holds a capture stream and an assembly pod).
	autoPauseParallel = 4

	pausedPausing  = "pausing"  // the capture is stored, its image is being assembled: the sandbox still runs
	pausedStopping = "stopping" // the image is ready, the sandbox is being deleted
	pausedResuming = "resuming"
	// pauseLease: a pause's driver (finishPause) renews its lease every pauseRenew; one left to lapse is a stopped
	// portal's, and the autoPause tick takes the pause over (redrivePauses).
	pauseLease = 2 * time.Minute
	pauseRenew = 30 * time.Second
	// pauseHold: how long past its start a pause keeps its sandbox from its end: the image wait, then the delete.
	pauseHold = snapshotWait + 10*time.Minute
	// pauseRetryEnd: the least a failed pause leaves its sandbox to run, so an autoPause one past its end is not
	// removed at once but paused again by the next tick.
	pauseRetryEnd = 5 * time.Minute
	resumeLease   = snapshotWait + time.Duration(sandbox.MaxStartupSeconds)*time.Second + 2*time.Minute // covers waiting for a snapshot still being assembled
)

// The E2B SDK gives up on a request after 120 s (request_timeout 60 s, taken as read + write by its transport), and a
// request it left is lost work. These bound what a request waits for; vars so the integration tests can shorten them.
var (
	// PauseJoinWait: how long a connect, resume or wake waits for a pause whose image is still being assembled before
	// it cancels that pause and answers with the sandbox, which still runs and holds every file.
	PauseJoinWait = 45 * time.Second
	// SnapshotRequestWait: how long after it arrived a fork or a create from a snapshot waits for the snapshot image;
	// past it the call answers 503 (retry) and leaves e2bReadyTimeout for the start.
	SnapshotRequestWait = 60 * time.Second
	// ForkReuseWindow: how long after a fork arrived its snapshot serves a retry of that fork (the same sandbox in the
	// same project) instead of a new capture, so a client retrying the 503 gets its forks once the image is ready on a
	// site where the assembly takes longer than SnapshotRequestWait. Job sandboxBuildSync drops one left unused.
	ForkReuseWindow = 5 * time.Minute
)

// SetSandboxPaused wires the sandbox_paused collection (nil = pause and resume off).
func (h *Handler) SetSandboxPaused(s *pgdoc.Store) { h.sbPaused = s }

// pausedSandbox is a paused sandbox: its saved CR and the snapshot template it resumes from. Keyed by the sandbox id.
type pausedSandbox struct {
	ID         string         `json:"id"`
	ProjectID  string         `json:"projectId"`
	ServiceID  string         `json:"serviceId"`
	TemplateID string         `json:"templateId"` // the internal snapshot template
	Saved      map[string]any `json:"saved"`      // the CR as it was (sandbox.SavedSandbox)
	PausedAt   *time.Time     `json:"pausedAt"`
	// State is "" once paused, "pausing" while its image is assembled (the sandbox runs), "stopping" while the sandbox
	// is deleted, "resuming" while a resume brings it back.
	// LeaseUntil bounds a transition: an expired one is a crashed caller's, and the record counts as paused again (or,
	// for a pause, as never paused).
	State           string     `json:"state,omitempty"`
	LeaseUntil      *time.Time `json:"leaseUntil,omitempty"`
	ResumeUncertain bool       `json:"resumeUncertain,omitempty"`
	// Automatic pauses must reauthorize source deletion after the asynchronous
	// image assembly, including after a portal restart/redrive.
	AbuseGuard *abusePauseGuard `json:"abuseGuard,omitempty"`
}

// e2bStatusErr is an error with the HTTP status the E2B API answers it with (e2bError reads it).
type e2bStatusErr struct {
	code int
	msg  string
}

func (e e2bStatusErr) Error() string { return e.msg }

var (
	errSandboxBusy      = e2bStatusErr{http.StatusConflict, "the sandbox is being paused or resumed; retry in a few seconds"}
	errAlreadyRunning   = e2bStatusErr{http.StatusConflict, "the sandbox is already running"}
	errAlreadyPaused    = e2bStatusErr{http.StatusConflict, "the sandbox is already paused"}
	errSnapshotsOff     = e2bStatusErr{http.StatusServiceUnavailable, "pause and snapshots are not available at this location"}
	errMemorySnapshot   = e2bStatusErr{http.StatusBadRequest, "memory snapshots (mode \"full\") are not supported: pause or snapshot with mode \"filesystem\"; running processes are not kept"}
	errSnapshotNotReady = e2bStatusErr{http.StatusServiceUnavailable, "the snapshot is still being assembled; retry in a few seconds"}
	errSnapshotGone     = errors.New("snapshot not found")
)

func (p *pausedSandbox) leased(now time.Time) bool {
	return p.State != "" && p.LeaseUntil != nil && now.Before(*p.LeaseUntil)
}

// snapshotsOn: this location can take snapshots (the stores are wired, and it has a template registry and a build
// bucket for the assembly).
func (h *Handler) snapshotsOn(c *e2bCaller) error {
	if !h.snapshotsAvailable(c.ss.Config()) {
		return errSnapshotsOff
	}
	return nil
}

func (h *Handler) snapshotsAvailable(cfg sandbox.Config) bool {
	return h.sbTemplates != nil && h.sbPaused != nil && buildsOn(cfg)
}

// updatePaused changes the paused record under a row lock. fn returns an error to abort (nothing is written); remove
// asks for the record to be deleted.
func (h *Handler) updatePaused(ctx context.Context, projectID, id string, fn func(p *pausedSandbox) (remove bool, err error)) error {
	return h.sbPaused.DB().WithTx(context.WithoutCancel(ctx), func(tc context.Context) error {
		if err := h.lockPaused(tc, id); err != nil {
			return err
		}
		var p pausedSandbox
		found, err := h.sbPaused.GetForUpdate(tc, id, &p)
		if err != nil {
			return err
		}
		if !found || p.ProjectID != projectID {
			return sandbox.ErrNotFound
		}
		remove, err := fn(&p)
		if err != nil {
			return err
		}
		if remove {
			_, err = h.sbPaused.DeleteByID(tc, id)
			return err
		}
		return h.sbPaused.Upsert(tc, id, &p)
	})
}

// lockPaused serialises every transition of one sandbox id, also before its record exists.
func (h *Handler) lockPaused(tc context.Context, id string) error {
	return h.sbPaused.DB().LockXact(tc, "sandbox_paused:"+id)
}

// waitGone waits until the sandbox's CR is gone (a pause or a kill deleting it), so a resume never starts while the
// old pod still holds the id.
func waitGone(ctx context.Context, c *e2bCaller, id string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		if _, err := c.ss.GetSandbox(ctx, c.proj.ID, id); errors.Is(err, sandbox.ErrNotFound) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// memoryFlag reads E2B's optional {memory, name} body. Absent or false = filesystem; true = 400.
func memoryFlag(w http.ResponseWriter, r *http.Request) (name string, ok bool) {
	var b struct {
		Memory *bool  `json:"memory"`
		Name   string `json:"name"`
	}
	if !decodeE2B(w, r, &b) {
		return "", false
	}
	if b.Memory != nil && *b.Memory {
		e2bErr(w, errMemorySnapshot.code, errMemorySnapshot.msg)
		return "", false
	}
	return b.Name, true
}

// ---- snapshots -------------------------------------------------------------------------------------------------

// snapshotSandbox captures the sandbox's filesystem changes and starts assembling the snapshot image, returning the
// snapshot template with its build still "building". The capture (exec + upload, seconds) happens in the request; the
// assembly (a pod running crane append, tens of seconds) finishes in the background, so create_snapshot answers fast
// and survives the client going away; a pause waits for it before deleting the sandbox. awaitSnapshot waits for (or
// completes) it.
// internal = a pause's or fork's snapshot (hidden from GET /snapshots and /templates); forkUntil: a fork's, reused by a
// retry of that fork until then.
func (h *Handler) snapshotSandbox(ctx context.Context, c *e2bCaller, id, name string, internal bool, forkUntil *time.Time) (out *sandboxTemplateDoc, err error) {
	if h.sbSafety == nil || ctx.Value(captureLockKey{}) == id {
		return h.snapshotSandboxWork(ctx, c, id, name, internal, forkUntil)
	}
	_, err = h.sbSafety.DB().WithAdvisoryLock(ctx, "sandbox-capture:"+c.es.ID+":"+c.proj.ID+":"+id, false, func() error {
		var e error
		out, e = h.snapshotSandboxWork(ctx, c, id, name, internal, forkUntil)
		return e
	})
	return
}
func (h *Handler) snapshotSandboxWork(ctx context.Context, c *e2bCaller, id, name string, internal bool, forkUntil *time.Time) (*sandboxTemplateDoc, error) {
	if err := h.snapshotsOn(c); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, snapshotWait)
	defer cancel()
	saved, err := c.ss.SavedSandbox(ctx, c.proj.ID, id)
	if err != nil {
		return nil, err
	}
	cfg := c.ss.Config()
	tm := sandbox.TemplateOfSaved(saved)
	now := time.Now().UTC()
	var t *sandboxTemplateDoc
	if name != "" {
		if !templateTagRe.MatchString(name) {
			return nil, e2bStatusErr{http.StatusBadRequest, fmt.Sprintf("invalid snapshot name %q", name)}
		}
		if _, builtin := cfg.Templates[name]; builtin {
			return nil, e2bStatusErr{http.StatusConflict, fmt.Sprintf("%q is a built-in template name", name)}
		}
		if t, err = h.loadTemplate(ctx, c.proj.ID, name); err != nil {
			return nil, err
		}
		if t != nil && (t.Kind != "snapshot" || t.Internal) {
			return nil, e2bStatusErr{http.StatusConflict, fmt.Sprintf("a template named %q exists", name)}
		}
	}
	created := false
	if t == nil {
		if name == "" {
			name = "snapshot-" + strings.TrimPrefix(id, "sbx-") + "-" + newToken(3)
		} else if n, err := h.sbTemplates.Count(ctx, pgdoc.M{"projectId": c.proj.ID, "internal": pgdoc.M{"$ne": true}}); err != nil {
			return nil, err
		} else if n >= maxTemplatesPerProject {
			return nil, e2bStatusErr{http.StatusTooManyRequests, fmt.Sprintf("at most %d templates per project", maxTemplatesPerProject)}
		}
		t = &sandboxTemplateDoc{ID: newTemplateID(), ProjectID: c.proj.ID, Name: name, Tags: map[string]string{},
			UploadToken: newToken(24), CreatedAt: &now, Kind: "snapshot", Internal: internal, SourceSandbox: id, ForkUntil: forkUntil}
		if _, err := h.sbTemplates.InsertOne(ctx, pgdoc.M{"_id": t.ID, "projectId": t.ProjectID, "name": name}); err != nil {
			return nil, err
		}
		created = true
		if err := h.saveTemplate(ctx, t); err != nil {
			return nil, err
		}
	}
	bs, err := buildStorage(cfg)
	if err != nil {
		return nil, err
	}
	buildID := uuid.NewString()
	key := contextKey(t.ProjectID, t.ID, snapshotHash(buildID))
	undo := func(cause error) (*sandboxTemplateDoc, error) {
		_, _ = bs.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bs.bucket, Key: &key})
		if created {
			_, _ = h.sbTemplates.DeleteOne(ctx, pgdoc.M{"_id": t.ID, "projectId": t.ProjectID})
		}
		return nil, cause
	}

	// 1. Stream the filesystem changes into the bucket (the size is not known up front).
	t0 := time.Now()
	// The archive is read and counted as it is uploaded: a capture that is not a whole gzip tar fails here, before an
	// assembly is started (seen on the lab: a 191 MB layer crane could not read, "unexpected EOF"). The count is the
	// billed size.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(c.ss.SnapshotFS(ctx, c.proj.ID, id, pw)) }()
	vr, vw := io.Pipe()
	type checkResult struct {
		size int64
		err  error
	}
	checked := make(chan checkResult, 1)
	go func() {
		n, err := checkLayer(vr)
		_ = vr.CloseWithError(err) // unblocks the upload if the check stops reading early
		checked <- checkResult{n, err}
	}()
	err = bs.put(ctx, key, io.TeeReader(pr, vw))
	vw.CloseWithError(err)
	res := <-checked
	if err == nil && res.err != nil {
		err = fmt.Errorf("the capture is not a complete archive: %w", res.err)
	}
	if err != nil {
		_ = pr.CloseWithError(err)
		return undo(fmt.Errorf("snapshot upload: %w", err))
	}
	size := res.size
	slog.Info("e2b: snapshot captured", "sandbox", id, "template", t.ID, "bytes", size, "took", time.Since(t0))
	layerURL, err := bs.fetchURL(ctx, key)
	if err != nil {
		return undo(err)
	}

	// 2. Record the build, then append the layer onto the sandbox's image in the background.
	if t, err = h.loadTemplate(ctx, c.proj.ID, t.ID); err != nil || t == nil {
		return undo(fmt.Errorf("snapshot record: %v", err))
	}
	t.CPU, t.MemoryGiB, t.GPU = tm.CPU, tm.MemoryGiB, tm.GPU
	bl := templateBuild{BuildID: buildID, Status: sandbox.BuildBuilding, StartCmd: tm.StartCmd, ReadyCmd: tm.ReadyCmd,
		StartAsRoot: tm.StartAsRoot, User: tm.User, Workdir: tm.Workdir, From: sandbox.ImageOfSaved(saved),
		Tags: []string{"default"}, CreatedAt: &now, SizeBytes: size}
	t.Builds = append(t.Builds, bl)
	if err := h.saveTemplate(ctx, t); err != nil {
		return undo(err)
	}
	err = h.withSandboxStart(ctx, c.es.ID, func(tc context.Context) error {
		return c.ss.StartSnapshot(tc, sandbox.SnapshotRequest{
			ProjectID: t.ProjectID, TemplateID: t.ID, BuildID: buildID, Base: bl.From,
			Image: templateImage(cfg, t, buildID), LayerURL: layerURL,
		})
	})
	if err != nil {
		h.failSnapshot(ctx, t.ProjectID, t.ID, buildID, err)
		if created {
			_ = h.deleteTemplate(ctx, c.ss, t)
		}
		return nil, err
	}
	go func() {
		bctx, cancel := context.WithTimeout(context.Background(), snapshotWait)
		defer cancel()
		if _, err := h.awaitSnapshot(bctx, c, t.ID); err != nil && !errors.Is(err, errSnapshotGone) { // gone: deleted meanwhile
			slog.Error("e2b: snapshot assembly", "template", t.ID, "err", err)
		}
	}()
	return t, nil
}

func snapshotHash(buildID string) string { return "snapshot-" + buildID }

// checkLayer reads r to its end as a gzip tar (the gzip CRC and length included), reports whether it is whole, and
// returns how many compressed bytes it read.
func checkLayer(r io.Reader) (int64, error) {
	cr := &countReader{r: r}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		return cr.n, err
	}
	tr := tar.NewReader(gz)
	for {
		if _, err := tr.Next(); err == io.EOF {
			break
		} else if err != nil {
			return cr.n, err
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return cr.n, err
		}
	}
	if _, err = io.Copy(io.Discard, gz); err != nil { // past tar's end blocks: the gzip trailer is checked at EOF
		return cr.n, err
	}
	_, err = io.Copy(io.Discard, cr) // gzip stops at its trailer: count anything after it too
	return cr.n, err
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// failSnapshot records a failed snapshot build.
func (h *Handler) failSnapshot(ctx context.Context, projectID, templateID, buildID string, cause error) {
	_, _, _ = h.finishBuild(ctx, projectID, templateID, buildID, func(_ *sandboxTemplateDoc, b *templateBuild) {
		b.Status, b.Reason = sandbox.BuildError, cause.Error()
	})
}

// awaitSnapshot waits until the snapshot template's latest build is ready (or failed), finishing it from the assembly
// pod's result if nobody has yet. Idempotent: the background goroutine, a waiting resume or create, and the job
// sandboxBuildSync (after an API restart) may all finish it; only the one that records the result bills it. A failed
// read or record (the store, the cluster) is retried until ctx ends (errSnapshotNotReady): only a missing record is
// errSnapshotGone, and only a failed build is an error of its own.
func (h *Handler) awaitSnapshot(ctx context.Context, c *e2bCaller, templateID string) (*sandboxTemplateDoc, error) {
	for {
		t, err := h.loadTemplate(ctx, c.proj.ID, templateID)
		if err == nil && (t == nil || len(t.Builds) == 0) {
			return nil, errSnapshotGone
		}
		if err == nil {
			b := &t.Builds[len(t.Builds)-1]
			switch b.Status {
			case sandbox.BuildReady:
				return t, nil
			case sandbox.BuildError:
				return nil, fmt.Errorf("snapshot failed: %s", b.Reason)
			}
			if done, err := h.finishSnapshot(ctx, c, t, b, true); err == nil && done {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil, errSnapshotNotReady
		case <-time.After(2 * time.Second):
		}
	}
}

// finishSnapshot records a snapshot build once its assembly pod has finished (done = recorded, by this call or another
// one). markGone: a missing pod means the assembly failed. The finisher that records a ready build starts the storage
// billing; whichever records the result deletes the pod and the layer in the bucket.
func (h *Handler) finishSnapshot(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild, markGone bool) (bool, error) {
	st, err := c.ss.BuildPodState(ctx, t.ProjectID, b.BuildID)
	switch {
	case err != nil:
		return false, nil // the cluster may answer next time
	case st == nil && !markGone:
		return false, nil
	case st == nil:
		h.failSnapshot(ctx, t.ProjectID, t.ID, b.BuildID, errors.New("the snapshot assembly disappeared"))
		return true, nil
	case st.Status != sandbox.BuildReady && st.Status != sandbox.BuildError:
		return false, nil
	}
	h.captureBuildArchive(ctx, c, t, b)
	logs := b.Logs
	reg := strings.TrimRight(c.ss.Config().TemplateRegistry, "/")
	ft, changed, err := h.finishBuild(ctx, t.ProjectID, t.ID, b.BuildID, func(ft *sandboxTemplateDoc, fb *templateBuild) {
		fb.Logs = logs
		if st.Status == sandbox.BuildReady {
			fb.Status = sandbox.BuildReady
			fb.Image = fmt.Sprintf("%s/%s/%s@%s", reg, ft.ProjectID, ft.ID, st.Digest)
			ft.Tags["default"] = fb.BuildID
		} else {
			fb.Status, fb.Reason, fb.Step = sandbox.BuildError, st.Reason, st.Step
		}
	})
	if err != nil {
		return false, err
	}
	if changed {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if st.Status == sandbox.BuildReady {
			h.snapshotStorageRow(dctx, c, ft)
		}
		// The terminal worker survives until its bounded prefix archive is stable.
		if bs, err := buildStorage(c.ss.Config()); err == nil {
			key := contextKey(t.ProjectID, t.ID, snapshotHash(b.BuildID))
			_, _ = bs.client.DeleteObject(dctx, &s3.DeleteObjectInput{Bucket: &bs.bucket, Key: &key})
		}
	}
	return true, nil
}

// snapshotStorageRow bills the stored snapshot by size (SANDBOX_SNAPSHOT, hourly per GB) from the moment it is ready.
// A named snapshot taken again keeps its row and its start; the size is that of every ready build the record keeps
// (trimBuilds drops a superseded one, and the registry prune then deletes its image).
func (h *Handler) snapshotStorageRow(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc) {
	kind := "snapshot"
	if t.Internal {
		kind = "pause"
	}
	now := time.Now().UTC()
	var size int64
	for _, b := range t.Builds {
		if b.Status == sandbox.BuildReady {
			size += b.SizeBytes
		}
	}
	data := map[string]any{"name": t.Name, "kind": kind, "sizeGB": float64(size) / (1 << 30), "createdAt": now.Format(time.RFC3339)}
	if cr, _ := h.cloud.FindByServiceIDAndExternalID(ctx, c.es.ID, t.ID); cr != nil && cr.ProjectID == c.proj.ID {
		if prev, _ := cr.Data["createdAt"].(string); prev != "" {
			data["createdAt"] = prev
		}
		cr.Data, cr.UpdatedAt = data, &now
		if _, err := h.cloud.Update(ctx, cr); err != nil {
			slog.Error("e2b: snapshot storage row", "template", t.ID, "err", err)
		}
		return
	}
	region := c.proj.ServiceRegion(c.es.ID)
	if region == "" {
		region = c.es.SandboxRegion()
	}
	if _, err := h.cloud.Insert(ctx, &cloud.CloudResource{ServiceID: c.es.ID, Region: region, ProjectID: c.proj.ID,
		Type: cloud.TypeSandboxSnapshot, ExternalID: t.ID, Data: data, CreatedAt: &now, UpdatedAt: &now}); err != nil {
		slog.Error("e2b: snapshot storage row", "template", t.ID, "err", err)
	}
}

// dropSnapshot removes a snapshot template: its record, assembly pod, layer and storage billing (deleteTemplate).
// The image stays in the registry while anything runs from it; the prune takes it after that.
func (h *Handler) dropSnapshot(ctx context.Context, c *e2bCaller, templateID string) {
	if h.sbTemplates == nil || templateID == "" {
		return
	}
	t, err := h.loadTemplate(ctx, c.proj.ID, templateID)
	if err != nil || t == nil {
		return
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// Internal snapshot teardown owns the terminal assembly worker even if its
	// output archive was awaiting recapture. Remove it before its last DB owner.
	for _, b := range t.Builds {
		if buildPinActive(b) {
			return
		}
		if err := c.ss.DeleteBuild(cleanup, b.BuildID); err != nil {
			return
		}
	}
	if err := h.deleteTemplate(cleanup, c.ss, t); err != nil {
		slog.Error("e2b: drop snapshot", "template", templateID, "err", err)
	}
}

// ---- pause / resume --------------------------------------------------------------------------------------------

func (h *Handler) loadPaused(ctx context.Context, projectID, id string) *pausedSandbox {
	if h.sbPaused == nil || !sandbox.ValidID(id) {
		return nil
	}
	var p pausedSandbox
	if found, err := h.sbPaused.Get(ctx, id, &p); err != nil || !found || p.ProjectID != projectID {
		return nil
	}
	return &p
}

// pauseSandbox captures a running sandbox's filesystem, saves its CR as "pausing" and answers: from here on it lists as
// paused. It keeps running (and billing) while finishPause, in the background, waits for its snapshot image; only an
// image that is pushed lets the sandbox be deleted, and a failed assembly drops the pause, so the sandbox runs on and
// a pause never loses files it answered 204 for. Files written after the capture are not kept.
func (h *Handler) pauseSandbox(ctx context.Context, c *e2bCaller, id string) error {
	if h.sbSafety != nil && ctx.Value(captureLockKey{}) != id {
		_, err := h.sbSafety.DB().WithAdvisoryLock(ctx, "sandbox-capture:"+c.es.ID+":"+c.proj.ID+":"+id, false, func() error { return h.pauseSandbox(context.WithValue(ctx, captureLockKey{}, id), c, id) })
		return err
	}
	if err := h.snapshotsOn(c); err != nil {
		return err
	}
	if p := h.loadPaused(ctx, c.proj.ID, id); p != nil && (p.State == "" || p.leased(time.Now())) {
		if p.State == pausedResuming {
			return errSandboxBusy
		}
		return errAlreadyPaused // also while pausing: autoPause runs every minute, it must not capture again
	}
	saved, err := c.ss.SavedSandbox(ctx, c.proj.ID, id)
	if err != nil {
		return err
	}
	t, err := h.snapshotSandbox(ctx, c, id, "", true, nil)
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx) // the capture is stored: record it whatever the client does
	now := time.Now().UTC()
	d := &pauseDrive{id: id, templateID: t.ID, lease: now.Add(pauseLease)}
	// Claim the id: a resume seeing "pausing" waits (or cancels the pause), a kill ends it.
	err = h.sbPaused.DB().WithTx(ctx, func(tc context.Context) error {
		if err := h.lockPaused(tc, id); err != nil {
			return err
		}
		var cur pausedSandbox
		found, err := h.sbPaused.GetForUpdate(tc, id, &cur)
		if err != nil {
			return err
		}
		switch {
		case found && cur.ProjectID != c.proj.ID:
			return sandbox.ErrNotFound
		case found && cur.leased(now):
			return errSandboxBusy
		case found && cur.State == "":
			return errAlreadyPaused
		}
		// A kill that came during the capture deleted the sandbox: it must not come back as paused. A kill takes this
		// lock too (e2bKill), so none can land between this check and the record.
		if err := c.ss.Alive(ctx, c.proj.ID, id); err != nil {
			return err
		}
		if cr := h.e2bRow(ctx, c, id); cr != nil && cr.Data["status"] == sandbox.StatusDeleted {
			return sandbox.ErrNotFound
		}
		// No record, or an expired pause/resume of this (live) sandbox: take it over.
		if found && cur.TemplateID != t.ID {
			defer h.dropSnapshot(ctx, c, cur.TemplateID)
		}
		lease := d.lease
		return h.sbPaused.Upsert(tc, id, &pausedSandbox{ID: id, ProjectID: c.proj.ID, ServiceID: c.es.ID, TemplateID: t.ID,
			Saved: saved, PausedAt: &now, State: pausedPausing, LeaseUntil: &lease, AbuseGuard: abuseGuardFrom(ctx)})
	})
	if err != nil {
		h.dropSnapshot(ctx, c, t.ID)
		return err
	}
	// Hold the sandbox until finishPause is done with it: an end that comes while the image is assembled must not
	// remove the original before the copy is usable. abandonPause puts its own end back.
	if end, ok := savedShutdown(saved); ok && end.Before(now.Add(pauseHold)) {
		if err := c.ss.ShutdownAt(ctx, c.proj.ID, id, now.Add(pauseHold)); err != nil {
			h.abandonPause(ctx, c, d, err)
			return err
		}
	}
	go h.finishPause(c, d)
	return nil
}

// pauseDrive is one driver's hold on a pause (finishPause): the record is its own while it carries the drive's
// template and the lease the drive wrote last. Every step renews that lease.
type pauseDrive struct {
	id, templateID string
	mu             sync.Mutex
	lease          time.Time
}

// errPauseTaken: another driver holds the pause (it took over a lease this one let lapse).
var errPauseTaken = errors.New("another driver holds the pause")

// driveStep applies fn to the record if the drive still holds it and renews the lease; fn returns true to delete the
// record, and a State it sets to "" ends the transition (no lease). errPauseMoved: the pause was cancelled, killed or
// replaced; errPauseTaken: another driver holds it.
func (h *Handler) driveStep(ctx context.Context, c *e2bCaller, d *pauseDrive, fn func(p *pausedSandbox) (remove bool)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	lease := time.Now().UTC().Add(pauseLease)
	err := h.updatePaused(ctx, c.proj.ID, d.id, func(p *pausedSandbox) (bool, error) {
		switch {
		case p.TemplateID != d.templateID || p.State != pausedPausing && p.State != pausedStopping:
			return false, errPauseMoved
		case p.LeaseUntil == nil || !p.LeaseUntil.Equal(d.lease):
			return false, errPauseTaken
		}
		p.LeaseUntil = &lease
		remove := fn(p)
		if p.State == "" {
			p.LeaseUntil = nil
		}
		return remove, nil
	})
	if errors.Is(err, sandbox.ErrNotFound) {
		return errPauseMoved
	}
	if err == nil {
		d.lease = lease
	}
	return err
}

// renewPause keeps the drive's lease alive until ctx ends; a pause that moved on stops the drive's wait (stop).
func (h *Handler) renewPause(ctx context.Context, c *e2bCaller, d *pauseDrive, stop context.CancelFunc) {
	tick := time.NewTicker(pauseRenew)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		err := h.driveStep(ctx, c, d, func(*pausedSandbox) bool { return false })
		if errors.Is(err, errPauseMoved) || errors.Is(err, errPauseTaken) {
			stop()
			return
		}
	}
}

// redrivePauses takes over the pauses whose driver stopped renewing them (the portal restarted mid-pause) and finishes
// each as its driver would have, so a pause answered 204 never stays half-done, running and billing.
func (h *Handler) redrivePauses(ctx context.Context, es *externalservice.ExternalService, ss *sandbox.Service) {
	var ps []pausedSandbox
	if err := h.sbPaused.Find(ctx, pgdoc.M{"serviceId": es.ID, "state": pgdoc.M{"$in": []any{pausedPausing, pausedStopping}}}, &ps); err != nil {
		slog.Warn("autopause: list unfinished pauses", "service", es.ID, "err", err)
		return
	}
	for _, p := range ps {
		if p.leased(time.Now()) {
			continue
		}
		proj, err := h.svc.repo.FindByID(ctx, p.ProjectID)
		if err != nil || proj == nil {
			continue
		}
		c := &e2bCaller{proj: proj, es: es, ss: ss}
		d := &pauseDrive{id: p.ID, templateID: p.TemplateID}
		if err := h.updatePaused(ctx, proj.ID, p.ID, func(cur *pausedSandbox) (bool, error) {
			if cur.TemplateID != p.TemplateID || cur.State != pausedPausing && cur.State != pausedStopping || cur.leased(time.Now()) {
				return false, errPauseMoved
			}
			d.lease = time.Now().UTC().Add(pauseLease)
			cur.LeaseUntil = &d.lease
			return false, nil
		}); err != nil {
			continue
		}
		slog.Info("e2b: taking over an unfinished pause", "sandbox", p.ID, "project", p.ProjectID, "state", p.State)
		go h.finishPause(c, d)
	}
}

// savedShutdown is the time the cluster removes the sandbox saved (spec.shutdownTime); false when it has none.
func savedShutdown(saved map[string]any) (time.Time, bool) {
	spec, _ := saved["spec"].(map[string]any)
	s, _ := spec["shutdownTime"].(string)
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// errPauseMoved: the paused record is no longer the pause the caller holds (cancelled, killed, or further along).
var errPauseMoved = errors.New("the pause moved on")

// finishPause completes a pause once its snapshot image is pushed: the record goes to "stopping", the sandbox is
// deleted, the record to paused. A connect or a kill that ended the pause meanwhile leaves the sandbox alone. The image
// is followed until it is ready or its build fails, however old the pause (a re-drive after an outage too): past the
// pause's hold the cluster has removed the sandbox and the image is the only copy of its files. Only a failed build
// ends the pause, and the sandbox, if it still exists, runs on. It renews its lease all along; a portal that stops
// leaves the lease to lapse, and the autoPause tick runs this again (redrivePauses).
// ponytail: no time bound of its own; the build pod's activeDeadlineSeconds (BuildTimeout) ends a slow build, but one
// never scheduled keeps this drive (and its lease) alive.
func (h *Handler) finishPause(c *e2bCaller, d *pauseDrive) {
	id, templateID := d.id, d.templateID
	ctx := context.Background()
	held, moved := context.WithCancel(ctx) // ends when the pause moves on (renewPause)
	defer moved()
	rctx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	go h.renewPause(rctx, c, d, moved)
	ready, err := h.awaitSnapshot(held, c, templateID)
	if err == nil && ready.defaultBuild() == nil {
		err = errors.New("no ready build")
	}
	if err != nil {
		h.abandonPause(ctx, c, d, err)
		return
	}
	// Automatic pauses release their capture lock while assembling the image.
	// Reacquire it before the final authorization and deletion so a native exec
	// cannot begin between those two steps. Manual/E2B pauses retain their behavior.
	finish := func() error { return h.stopPausedSource(ctx, c, d) }
	var p pausedSandbox
	found, readErr := h.sbPaused.Get(ctx, id, &p)
	if readErr != nil {
		slog.Warn("sandbox pause: state unavailable; retaining source and copy", "sandbox", id)
		return
	}
	if !found || p.ProjectID != c.proj.ID {
		return
	}
	if p.AbuseGuard != nil {
		_, err = h.sbSafety.DB().WithAdvisoryLock(ctx, "sandbox-capture:"+c.es.ID+":"+c.proj.ID+":"+id, false, func() error {
			// If the source already expired, the usable image is the only copy;
			// no destructive action remains to authorize. Never discard that copy.
			if err := c.ss.Alive(ctx, c.proj.ID, id); errors.Is(err, sandbox.ErrNotFound) {
				return finish()
			}
			if err := h.authorizeAbuseAction(ctx, c.es.ID, p.AbuseGuard); err != nil {
				return err
			}
			return finish()
		})
	} else {
		err = finish()
	}
	if err != nil && !errors.Is(err, errPauseMoved) && !errors.Is(err, errPauseTaken) {
		h.abandonPause(ctx, c, d, err)
	}
}

func (h *Handler) stopPausedSource(ctx context.Context, c *e2bCaller, d *pauseDrive) error {
	id := d.id
	if err := h.driveStep(ctx, c, d, func(p *pausedSandbox) bool {
		p.State = pausedStopping
		return false
	}); err != nil {
		return err
	}
	var err error
	// A sandbox that is gone already (its end came while the image was assembled) is paused all the same: the image
	// holds its files.
	if _, gerr := c.ss.GetSandbox(ctx, c.proj.ID, id); !errors.Is(gerr, sandbox.ErrNotFound) {
		if cr := h.e2bRow(ctx, c, id); cr != nil {
			err = h.sandboxDelete(ctx, c.es, c.proj, cr)
		} else {
			err = c.ss.DeleteSandbox(ctx, c.proj.ID, id)
		}
		if err != nil { // the image is ready: the pause is given up only while the original still holds the files
			if _, gerr := c.ss.GetSandbox(ctx, c.proj.ID, id); !errors.Is(gerr, sandbox.ErrNotFound) {
				return err
			}
		}
		waitGone(ctx, c, id, time.Minute)
	}
	if err := h.driveStep(ctx, c, d, func(p *pausedSandbox) bool {
		p.State = ""
		return false
	}); err != nil {
		slog.Warn("e2b: pause done, its record moved on", "sandbox", id, "err", err)
		return err
	}
	slog.Info("e2b: paused", "sandbox", id, "project", c.proj.ID, "template", d.templateID)
	return nil
}

// abandonPause ends a pause that cannot complete: the record (if the drive still holds it) and the snapshot go, the
// sandbox keeps running, to its own end again, or a few minutes when that has passed (an autoPause one is then paused
// again by the next tick instead of removed at once).
func (h *Handler) abandonPause(ctx context.Context, c *e2bCaller, d *pauseDrive, cause error) {
	// A guarded pause may be abandoned only when the original is positively
	// known alive. An outage/expired source can leave this image as the only
	// copy; retain its record for redrive instead of treating unknown as alive.
	var current pausedSandbox
	found, readErr := h.sbPaused.Get(ctx, d.id, &current)
	if readErr != nil {
		return
	} // unknown ownership/state never authorizes deleting a copy
	if found && current.AbuseGuard != nil {
		if err := c.ss.OriginalSourceAlive(ctx, current.AbuseGuard.Sample); err != nil {
			slog.Warn("sandbox abuse: retaining pause copy; original source not known alive", "sandbox", d.id)
			return
		}
	}
	var saved map[string]any
	var identityChanged bool
	err := h.driveStep(ctx, c, d, func(p *pausedSandbox) bool {
		// Recheck at the owning-record mutation too: the source may have been
		// replaced since the outer liveness read.
		if p.AbuseGuard != nil && c.ss.OriginalSourceAlive(ctx, p.AbuseGuard.Sample) != nil {
			identityChanged = true
			return false
		}
		saved = p.Saved
		return true
	})
	if identityChanged {
		return
	}
	if errors.Is(err, errPauseTaken) {
		return // another driver carries the pause on, with this snapshot
	}
	if err != nil {
		return // do not delete a snapshot whose owning record could not be removed
	}
	h.dropSnapshot(ctx, c, d.templateID)
	if end, ok := savedShutdown(saved); ok {
		end = laterOf(end, time.Now().Add(pauseRetryEnd))
		if serr := c.ss.ShutdownAt(ctx, c.proj.ID, d.id, end); serr != nil && !errors.Is(serr, sandbox.ErrNotFound) {
			slog.Warn("e2b: restoring the end of a sandbox whose pause failed", "sandbox", d.id, "err", serr)
		}
	}
	slog.Error("e2b: pause failed, the sandbox keeps running", "sandbox", d.id, "project", c.proj.ID, "err", cause)
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// cancelPause ends a pause whose image is not ready yet, so a connect gets the sandbox (which still runs and holds
// every file) at once. false: there is no such pause (it is gone, or its sandbox is already being deleted), or the
// sandbox no longer runs: the snapshot is then the only copy of its files, so the pause goes on and the connect waits.
func (h *Handler) cancelPause(ctx context.Context, c *e2bCaller, id string) bool {
	var tpl string
	if err := h.updatePaused(ctx, c.proj.ID, id, func(p *pausedSandbox) (bool, error) {
		if p.State != pausedPausing {
			return false, errPauseMoved
		}
		if c.ss.Alive(ctx, c.proj.ID, id) != nil {
			return false, errPauseMoved
		}
		if p.AbuseGuard != nil && c.ss.OriginalSourceAlive(ctx, p.AbuseGuard.Sample) != nil {
			return false, errSandboxBusy // a same-name replacement does not hold the captured files
		}
		tpl = p.TemplateID
		return true, nil
	}); err != nil {
		return false
	}
	h.dropSnapshot(context.WithoutCancel(ctx), c, tpl)
	slog.Info("e2b: pause cancelled by a connect", "sandbox", id, "project", c.proj.ID)
	return true
}

// POST /sandboxes/{sandboxID}/pause {memory}
func (h *Handler) e2bPause(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	id := chi.URLParam(r, "sandboxID")
	if _, ok := memoryFlag(w, r); !ok {
		return
	}
	if err := h.pauseSandbox(r.Context(), c, id); err != nil {
		h.e2bSnapshotFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) e2bSnapshotFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sandbox.ErrNotRunning):
		e2bErr(w, http.StatusConflict, "the sandbox is not running")
	case errors.Is(err, sandbox.ErrBadBuild):
		e2bErr(w, http.StatusBadRequest, err.Error())
	default:
		e2bFail(w, err)
	}
}

// startSaved creates a sandbox from a saved CR on image (resume or fork), under the project quota, and waits for it.
func (h *Handler) startSaved(ctx context.Context, c *e2bCaller, saved map[string]any, image string, timeoutSeconds int) (string, error) {
	if _, err := sandbox.ShutdownTimeFor(c.ss.Config(), time.Now(), timeoutSeconds); err != nil {
		return "", e2bStatusErr{http.StatusBadRequest, err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, c.ss.Config().Startup.Duration()+time.Minute)
	defer cancel()
	sandbox.ArmSavedStartup(saved, c.ss.Config(), timeoutSeconds, time.Now())
	md, _ := saved["metadata"].(map[string]any)
	id, _ := md["name"].(string)
	spec, _ := saved["spec"].(map[string]any)
	shutdown, err := time.Parse(time.RFC3339, fmt.Sprint(spec["shutdownTime"]))
	if err != nil {
		return "", err
	}

	q := c.es.SandboxProjectQuota(c.proj.Quota)
	if err := h.withSandboxStart(ctx, c.es.ID, func(tc context.Context) error { return c.ss.ResumeSandbox(tc, c.proj.ID, saved, image, shutdown, &q) }); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	// A new billing row: the old one (DELETED at the pause) is archived, and the charge rates archived rows to their end.
	if old := h.e2bRow(ctx, c, id); old != nil {
		if old.Data["status"] != sandbox.StatusDeleted {
			sandbox.MarkVanished(old, now)
		}
		if err := h.cloud.DeleteAndArchive(ctx, old, now); err != nil {
			slog.Warn("e2b: archive the row of the paused sandbox", "sandbox", id, "err", err)
		}
	}
	region := c.proj.ServiceRegion(c.es.ID)
	if region == "" {
		region = c.es.SandboxRegion()
	}
	row, err := h.cloud.Insert(ctx, &cloud.CloudResource{ServiceID: c.es.ID, Region: region, ProjectID: c.proj.ID,
		Type: cloud.TypeSandbox, ExternalID: id, Data: sandbox.PayloadOf(saved), CreatedAt: &now, UpdatedAt: &now})
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = c.ss.DeleteSandbox(cleanup, c.proj.ID, id)
		cancel()
		return "", err
	}
	if err := h.e2bWaitReady(ctx, c, row); err != nil {
		h.cleanupFailedStartup(ctx, c, row)
		return "", e2bStatusErr{http.StatusServiceUnavailable, "the sandbox did not start in time"}
	}
	if err := startupResponseAlive(ctx, row.Data); err != nil {
		h.cleanupFailedStartup(ctx, c, row)
		return "", err
	}
	return id, nil
}

// resumePaused brings a paused sandbox back under its id and returns its E2B Sandbox. It passes the usage gate first:
// a suspended project or profile keeps its snapshot but cannot bring it back (D9).
func (h *Handler) resumePaused(ctx context.Context, c *e2bCaller, p *pausedSandbox, timeoutSeconds int) (map[string]any, error) {
	if err := h.sandboxUsageGate(ctx, c.proj, c.es); err != nil {
		return nil, err
	}
	if _, err := sandbox.ShutdownTimeFor(c.ss.Config(), time.Now(), timeoutSeconds); err != nil {
		return nil, e2bStatusErr{http.StatusBadRequest, err.Error()}
	}
	// Claim the resume: exactly one caller brings the sandbox back; a concurrent one waits for it and answers with the
	// same sandbox; a pause under way is waited for, and one whose image is still not ready after PauseJoinWait is
	// cancelled: the sandbox still runs, so the caller gets it as it is.
	joinUntil := time.Now().Add(PauseJoinWait)
	for attempt := 0; ; attempt++ {
		out, err := h.claimResume(ctx, c.proj.ID, p)
		if errors.Is(err, sandbox.ErrNotFound) || out == claimJoined {
			return h.awaitResumed(ctx, c, p.ID) // resumed (or killed) by someone else
		}
		if err != nil {
			return nil, err
		}
		if attempt > 70 {
			return nil, errSandboxBusy
		}
		switch out {
		case claimPausing:
			if time.Now().After(joinUntil) && h.cancelPause(ctx, c, p.ID) {
				// It runs on as a resumed one would: for timeoutSeconds from now, not to the pause's hold.
				if _, err := c.ss.SetTimeout(ctx, c.proj.ID, p.ID, timeoutSeconds); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
					return nil, err
				}
				return h.awaitResumed(ctx, c, p.ID)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		case claimStaleResume:
			// Lease age and Alive/absence are not proof that a prior apply had no effect.
			return nil, errSandboxBusy
		case claimStalePause:
			if c.ss.Alive(ctx, c.proj.ID, p.ID) == nil {
				// The crashed pause never removed the sandbox, or the crashed resume already started it: it runs.
				// Finish by dropping the pause (unless a driver took it over meanwhile).
				dctx := context.WithoutCancel(ctx)
				var tpl string
				if err := h.updatePaused(dctx, c.proj.ID, p.ID, func(cur *pausedSandbox) (bool, error) {
					if cur.State == "" || cur.leased(time.Now()) {
						return false, errPauseMoved
					}
					if cur.AbuseGuard != nil && c.ss.OriginalSourceAlive(dctx, cur.AbuseGuard.Sample) != nil {
						return false, errSandboxBusy // never adopt a replacement and discard the original files
					}
					tpl = cur.TemplateID
					return true, nil
				}); err != nil {
					if errors.Is(err, errSandboxBusy) {
						return nil, err
					}
					continue
				}
				h.dropSnapshot(dctx, c, tpl)
				// It runs on as a resumed one would: for timeoutSeconds from now, not to the dead pause's hold.
				if _, err := c.ss.SetTimeout(ctx, c.proj.ID, p.ID, timeoutSeconds); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
					return nil, err
				}
				return h.awaitResumed(ctx, c, p.ID)
			}
			_ = h.updatePaused(ctx, c.proj.ID, p.ID, func(cur *pausedSandbox) (bool, error) {
				if !cur.leased(time.Now()) {
					cur.State, cur.LeaseUntil = "", nil
				}
				return false, nil
			})
			continue
		}
		break // claimed
	}
	// The claim holds for resumeLease: give up before it lapses, so a second caller can never take over a resume
	// that is still starting the sandbox (and archive its billing row).
	ctx, cancel := context.WithTimeout(ctx, resumeLease-time.Minute)
	defer cancel()
	release := func(cause error) (map[string]any, error) {
		_ = h.updatePaused(context.WithoutCancel(ctx), c.proj.ID, p.ID, func(cur *pausedSandbox) (bool, error) {
			if cur.State == pausedResuming && !cur.ResumeUncertain {
				cur.State, cur.LeaseUntil = "", nil
			}
			return false, nil
		})
		return nil, cause
	}
	if !waitGone(ctx, c, p.ID, 30*time.Second) {
		return release(errSandboxBusy)
	}
	t, err := h.awaitSnapshot(ctx, c, p.TemplateID)
	if err != nil {
		if errors.Is(err, errSnapshotNotReady) {
			return release(err)
		}
		return release(e2bStatusErr{http.StatusInternalServerError, "the paused sandbox's snapshot is not usable: " + err.Error()})
	}
	if t.defaultBuild() == nil {
		return release(e2bStatusErr{http.StatusInternalServerError, "the paused sandbox's snapshot is not usable"})
	}
	// Publish the no-takeover marker before the external apply. Any ambiguous
	// launch/cleanup response retains this owner and snapshot for operator recovery.
	if err := h.updatePaused(ctx, c.proj.ID, p.ID, func(cur *pausedSandbox) (bool, error) {
		if cur.State != pausedResuming || cur.ResumeUncertain {
			return false, errSandboxBusy
		}
		cur.ResumeUncertain = true
		return false, nil
	}); err != nil {
		return release(err)
	}
	id, err := h.startSaved(ctx, c, p.Saved, t.defaultBuild().Image, timeoutSeconds)
	if err != nil {
		return release(err)
	}
	// Access is part of usable success. Do not drop the retryable snapshot or
	// lease while a late/failed access read could leave the caller with no sandbox.
	a, err := h.savedStartupAccess(ctx, c, id)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dctx, cancelDone := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelDone()
	if _, err := h.sbPaused.DeleteByID(dctx, p.ID); err != nil {
		return nil, err
	}
	h.dropSnapshot(dctx, c, p.TemplateID)
	tpl, _ := sandbox.PayloadOf(p.Saved)["template"].(string)
	return c.e2bSandbox(id, tpl, a), nil
}

// POST /sandboxes/{sandboxID}/resume {timeout}
func (h *Handler) e2bResume(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	var b struct {
		Timeout *int  `json:"timeout"`
		Memory  *bool `json:"memory"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	id := chi.URLParam(r, "sandboxID")
	p := h.loadPaused(r.Context(), c.proj.ID, id)
	if p == nil {
		if _, err := c.ss.GetSandbox(r.Context(), c.proj.ID, id); err == nil {
			e2bErr(w, errAlreadyRunning.code, errAlreadyRunning.msg)
		} else {
			e2bErr(w, http.StatusNotFound, "sandbox not found")
		}
		return
	}
	timeout := 300
	if b.Timeout != nil {
		timeout = *b.Timeout
	}
	sb, err := h.resumePaused(r.Context(), c, p, timeout)
	if err != nil {
		e2bFail(w, err)
		return
	}
	e2bJSON(w, http.StatusCreated, sb)
}

// pausedListed renders a paused sandbox as ListedSandbox / SandboxDetail.
func pausedListed(p *pausedSandbox) map[string]any {
	out := e2bListed(p.ID, sandbox.PayloadOf(p.Saved))
	out["state"] = "paused"
	if p.PausedAt != nil {
		out["endAt"] = p.PausedAt.Format(time.RFC3339)
		if out["startedAt"] == "" { // the saved CR has no status: the SDK requires a timestamp
			out["startedAt"] = out["endAt"]
		}
	}
	return out
}

func (h *Handler) projectPaused(ctx context.Context, projectID string) []pausedSandbox {
	if h.sbPaused == nil {
		return nil
	}
	var ps []pausedSandbox
	if err := h.sbPaused.Find(ctx, pgdoc.M{"projectId": projectID}, &ps); err != nil {
		slog.Warn("e2b: list paused sandboxes", "project", projectID, "err", err)
	}
	return ps
}

// ---- snapshots / fork ------------------------------------------------------------------------------------------

// POST /sandboxes/{sandboxID}/snapshots {name, memory}
func (h *Handler) e2bSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	name, ok := memoryFlag(w, r)
	if !ok {
		return
	}
	// A stored snapshot is billed: the same gate as a create.
	if err := h.sandboxUsageGate(r.Context(), c.proj, c.es); err != nil {
		e2bFail(w, err)
		return
	}
	t, err := h.snapshotSandbox(r.Context(), c, chi.URLParam(r, "sandboxID"), name, false, nil)
	if err != nil {
		h.e2bSnapshotFail(w, err)
		return
	}
	e2bJSON(w, http.StatusCreated, map[string]any{"snapshotID": t.ID, "names": []string{t.Name}})
}

// GET /snapshots?sandboxID=&name=
func (h *Handler) e2bSnapshotList(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	q := r.URL.Query()
	out := []any{}
	for _, t := range h.projectTemplates(r.Context(), c.proj.ID) {
		if t.Kind != "snapshot" || len(t.Builds) == 0 || t.defaultBuild() == nil && t.Builds[len(t.Builds)-1].Status == sandbox.BuildError {
			continue
		}
		if s := q.Get("sandboxID"); s != "" && t.SourceSandbox != s {
			continue
		}
		if n := q.Get("name"); n != "" && t.Name != n {
			continue
		}
		out = append(out, map[string]any{"snapshotID": t.ID, "names": []string{t.Name}})
	}
	e2bJSON(w, http.StatusOK, out)
}

// POST /sandboxes/{sandboxID}/fork {timeout, count}
func (h *Handler) e2bFork(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	var b struct {
		Timeout *int `json:"timeout"`
		Count   *int `json:"count"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	count, timeout := 1, 15 // E2B's defaults
	if b.Count != nil {
		count = *b.Count
	}
	if b.Timeout != nil {
		timeout = *b.Timeout
	}
	if count < 1 || count > maxForkCount {
		e2bErr(w, http.StatusBadRequest, fmt.Sprintf("count must be between 1 and %d", maxForkCount))
		return
	}
	if _, err := sandbox.ShutdownTimeFor(c.ss.Config(), time.Now(), timeout); err != nil {
		e2bErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, arrived := r.Context(), time.Now()
	if err := h.sandboxUsageGate(ctx, c.proj, c.es); err != nil {
		e2bFail(w, err)
		return
	}
	if err := h.snapshotsOn(c); err != nil {
		e2bFail(w, err)
		return
	}
	live, err := c.ss.ListSandboxes(ctx, c.proj.ID)
	if err != nil {
		e2bFail(w, err)
		return
	}
	lim := c.ss.Config().WithDefaults().Limits.MaxConcurrent
	if q := c.es.SandboxProjectQuota(c.proj.Quota); q.Sandboxes < lim {
		lim = q.Sandboxes
	}
	if len(live)+count > lim {
		e2bErr(w, http.StatusTooManyRequests, fmt.Sprintf("%v: at most %d sandboxes may run at once", sandbox.ErrQuotaExceeded, lim))
		return
	}
	id := chi.URLParam(r, "sandboxID")
	saved, err := c.ss.SavedSandbox(ctx, c.proj.ID, id)
	if err != nil {
		h.e2bSnapshotFail(w, err)
		return
	}
	// A retry of a fork that answered 503 takes that fork's snapshot, still being assembled or ready, instead of a new
	// one: on a site where the assembly takes longer than SnapshotRequestWait, a new capture each time never finishes.
	t := h.pendingFork(ctx, c.proj.ID, id)
	if t == nil {
		until := arrived.Add(ForkReuseWindow)
		if t, err = h.snapshotSandbox(ctx, c, id, "", true, &until); err != nil {
			h.e2bSnapshotFail(w, err)
			return
		}
	}
	// Answer within the SDK's request timeout: a snapshot not ready by then is kept for a retry (job sandboxBuildSync
	// drops it past ForkReuseWindow) and the call answers 503; a failed one is dropped.
	wctx, cancel := context.WithDeadline(ctx, arrived.Add(SnapshotRequestWait))
	ready, err := h.awaitSnapshot(wctx, c, t.ID)
	cancel()
	if err == nil && ready.defaultBuild() == nil {
		err = errSnapshotNotReady
	}
	if errors.Is(err, errSnapshotGone) { // taken by a concurrent fork or past its window: a retry takes a new one
		err = errSnapshotNotReady
	}
	if err != nil {
		if !errors.Is(err, errSnapshotNotReady) {
			h.dropSnapshot(context.WithoutCancel(ctx), c, t.ID)
		}
		h.e2bSnapshotFail(w, err)
		return
	}
	t = ready
	image := t.defaultBuild().Image
	// The forks start in parallel: each waits up to e2bReadyTimeout for envd.
	results := make([]any, count)
	tpl, _ := sandbox.PayloadOf(saved)["template"].(string)
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			clone, _ := sandbox.CloneSaved(saved)
			newID, err := h.startSaved(ctx, c, clone, image, timeout)
			var a *sandbox.Access
			if err == nil {
				a, err = h.savedStartupAccess(ctx, c, newID)
			}
			if err != nil {
				code, msg := e2bError(err)
				results[i] = map[string]any{"error": map[string]any{"code": code, "message": msg}}
				return
			}
			results[i] = map[string]any{"sandbox": c.e2bSandbox(newID, tpl, a)}
		})
	}
	wg.Wait()
	// The forks run from the snapshot image; its template record is not needed any more.
	h.dropSnapshot(context.WithoutCancel(ctx), c, t.ID)
	if err := ctx.Err(); err != nil {
		e2bFail(w, err)
		return
	}
	e2bJSON(w, http.StatusCreated, results)
}

// pendingFork is a fork snapshot of the project's sandbox id still within its reuse window and not failed (nil: none).
// Keyed by project: another project's fork never sees it.
func (h *Handler) pendingFork(ctx context.Context, projectID, sourceID string) *sandboxTemplateDoc {
	var ts []sandboxTemplateDoc
	if err := h.sbTemplates.Find(ctx, pgdoc.M{"projectId": projectID, "sourceSandbox": sourceID, "internal": true}, &ts); err != nil {
		return nil
	}
	now := time.Now()
	for i := range ts {
		t := &ts[i]
		if t.ProjectID == projectID && t.ForkUntil != nil && now.Before(*t.ForkUntil) &&
			len(t.Builds) > 0 && t.Builds[len(t.Builds)-1].Status != sandbox.BuildError {
			return t
		}
	}
	return nil
}

// ---- autoPause -------------------------------------------------------------------------------------------------

// AutoPauseSandboxes pauses every auto-pause sandbox past its end (job sandboxAutoPause, every minute), after taking
// over the pauses a stopped portal left unfinished (redrivePauses). A sandbox whose pause fails runs on to its
// shutdownTime (end + sandbox.AutoPauseGrace), or pauseRetryEnd when that has passed, and a later tick pauses it again.
func (h *Handler) AutoPauseSandboxes(ctx context.Context) {
	if h.sandboxFor == nil || h.sbPaused == nil || h.sbTemplates == nil {
		return
	}
	svcs, err := h.esSvc.List(ctx)
	if err != nil {
		return
	}
	for i := range svcs {
		es := &svcs[i]
		if !es.IsSandbox() {
			continue
		}
		ss, err := h.sandboxFor(es)
		if err != nil || !buildsOn(ss.Config()) {
			continue
		}
		h.redrivePauses(ctx, es, ss) // before listing: a pause taken over is not captured again
		due, err := ss.DueForAutoPause(ctx)
		if err != nil {
			slog.Warn("autopause: list", "service", es.ID, "err", err)
			continue
		}
		// A pause streams its capture before it answers: a few at a time. One still assembling its image is past its end
		// and listed again: errAlreadyPaused, nothing to do.
		var wg sync.WaitGroup
		sem := make(chan struct{}, autoPauseParallel)
		for _, d := range due {
			proj, err := h.svc.repo.FindByID(ctx, d.ProjectID)
			if err != nil || proj == nil {
				continue
			}
			c := &e2bCaller{proj: proj, es: es, ss: ss}
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				if err := h.pauseSandbox(ctx, c, d.ID); errors.Is(err, errAlreadyPaused) {
					return
				} else if err != nil {
					slog.Error("autopause: pause", "sandbox", d.ID, "err", err)
					return
				}
				slog.Info("autopause: pausing", "sandbox", d.ID, "project", d.ProjectID)
			})
		}
		wg.Wait()
	}
}

// awaitIfSnapshot lets a create from a snapshot that is still being assembled wait for it until deadline, instead of
// failing with "no ready build"; still not ready then: errSnapshotNotReady (503, retry), and the assembly goes on. A
// failed or deleted snapshot is left to the create's own template check.
func (h *Handler) awaitIfSnapshot(ctx context.Context, c *e2bCaller, ref string, deadline time.Time) error {
	if h.sbTemplates == nil {
		return nil
	}
	name, _ := splitNameTag(ref)
	t, err := h.loadTemplate(ctx, c.proj.ID, name)
	if err != nil || t == nil || t.Kind != "snapshot" || t.defaultBuild() != nil {
		return nil
	}
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if _, err := h.awaitSnapshot(wctx, c, t.ID); errors.Is(err, errSnapshotNotReady) {
		return err
	}
	return nil
}

type claimOutcome int

const (
	claimed          claimOutcome = iota
	claimJoined                   // another caller is resuming it
	claimPausing                  // a pause is under way
	claimStalePause               // a pause crashed half-way (lease expired)
	claimStaleResume              // a resume crashed half-way (lease expired)
)

// claimResume takes the resume lease on a paused sandbox (filling *p with the record as claimed).
func (h *Handler) claimResume(ctx context.Context, projectID string, p *pausedSandbox) (out claimOutcome, err error) {
	err = h.updatePaused(ctx, projectID, p.ID, func(cur *pausedSandbox) (bool, error) {
		now := time.Now().UTC()
		switch {
		case cur.ResumeUncertain:
			out = claimJoined
			return false, nil
		case cur.State == pausedResuming && cur.leased(now):
			out = claimJoined
			return false, nil
		case (cur.State == pausedPausing || cur.State == pausedStopping) && cur.leased(now):
			out = claimPausing
			return false, nil
		case cur.State == pausedPausing || cur.State == pausedStopping:
			out = claimStalePause
			return false, nil
		case cur.State == pausedResuming:
			out = claimStaleResume
			return false, nil
		}
		lease := now.Add(resumeLease)
		cur.State, cur.LeaseUntil = pausedResuming, &lease
		*p = *cur
		out = claimed
		return false, nil
	})
	return out, err
}

// awaitResumed waits for another caller's resume of the sandbox to finish and answers like it.
func (h *Handler) awaitResumed(ctx context.Context, c *e2bCaller, id string) (map[string]any, error) {
	deadline := time.Now().Add(resumeLease)
	for {
		if p := h.loadPaused(ctx, c.proj.ID, id); p == nil {
			d, err := c.ss.WaitReady(ctx, c.proj.ID, id)
			if err != nil {
				return nil, err
			}
			a, err := c.ss.AccessFor(ctx, c.proj.ID, id)
			if err != nil {
				return nil, err
			}
			if err := startupResponseAlive(ctx, d); err != nil {
				return nil, err
			}
			tpl, _ := d["template"].(string)
			return c.e2bSandbox(id, tpl, a), nil
		} else if p.State != pausedResuming || !p.leased(time.Now()) {
			return nil, errSandboxBusy // the other resume failed; the caller may retry
		}
		if time.Now().After(deadline) {
			return nil, errSandboxBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// killPaused removes a paused sandbox: its record, then its snapshot (E2B kill of a paused sandbox); the caller then
// kills a sandbox that still runs (a pause still assembling). busy while a resume, or a pause deleting its sandbox,
// holds it.
func (h *Handler) killPaused(ctx context.Context, c *e2bCaller, p *pausedSandbox) error {
	err := h.updatePaused(ctx, c.proj.ID, p.ID, func(cur *pausedSandbox) (bool, error) {
		if cur.ResumeUncertain || cur.State != pausedPausing && cur.leased(time.Now()) { // a pause still assembling ends here; its sandbox runs
			return false, errSandboxBusy
		}
		return true, nil
	})
	if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		return err
	}
	h.dropSnapshot(ctx, c, p.TemplateID)
	return nil
}

// deleteProjectPaused forgets every paused sandbox of the project on the sandbox provider serviceID (project
// teardown; their snapshot templates go with teardownTemplates).
func (h *Handler) deleteProjectPaused(ctx context.Context, projectID, serviceID string) error {
	if h.sbPaused == nil {
		return nil
	}
	var errs []error
	for _, ps := range h.projectPaused(ctx, projectID) {
		if ps.ServiceID != "" && ps.ServiceID != serviceID {
			continue
		}
		if _, err := h.sbPaused.DeleteByID(ctx, ps.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- wake (autoResume) -----------------------------------------------------------------------------------------

// e2bWakeLimit bounds one wake: the snapshot wait plus a cold start. The proxy stops waiting earlier (its client gets
// 503 + Retry-After) while the wake carries on.
const e2bWakeLimit = resumeLease - time.Minute

// wakePaused resumes the paused sandbox id for the wake endpoint (sandbox_wake.go). The body's tokens must be the
// paused sandbox's own (envd's for the envd port, the traffic token for other ports when it has one), so only someone
// who could talk to the sandbox can wake it. Answers in the portal's envelope, which the proxy reads.
func (h *Handler) wakePaused(w http.ResponseWriter, r *http.Request, bearer, id string, port int, envdToken, trafficToken string) {
	notFound := func() { httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "sandbox not found") }
	if h.sbPaused == nil {
		notFound()
		return
	}
	var p pausedSandbox
	if found, err := h.sbPaused.Get(r.Context(), id, &p); err != nil || !found ||
		digAnn(p.Saved, sandbox.AnnotationAutoResume) != "true" {
		notFound()
		return
	}
	proj, err := h.svc.repo.FindByID(r.Context(), p.ProjectID)
	if err != nil || proj == nil {
		notFound()
		return
	}
	c, status, msg := h.e2bCallerFor(r.Context(), nil, proj)
	if c == nil {
		httpx.Err(w, status, status, msg)
		return
	}
	// The token of the sandbox's own location first: another location's proxy learns nothing about its tokens.
	if c.es.ID != p.ServiceID || !tokenEq(bearer, c.ss.Config().ProxyWakeToken) {
		httpx.Err(w, http.StatusUnauthorized, http.StatusUnauthorized, "invalid wake token")
		return
	}
	a := sandbox.AccessOf(p.Saved)
	if port == sandbox.EnvdPort {
		if !tokenEq(envdToken, a.EnvdAccessToken) {
			httpx.Err(w, http.StatusForbidden, http.StatusForbidden, "invalid access token")
			return
		}
	} else if a.TrafficAccessToken != "" && !tokenEq(trafficToken, a.TrafficAccessToken) {
		httpx.Err(w, http.StatusForbidden, http.StatusForbidden, "invalid traffic access token")
		return
	}
	// A suspended project or profile keeps the snapshot but is not woken (D9): 403, which the proxy passes on, rather
	// than a retry the client would repeat for as long as the suspension lasts.
	if err := h.sandboxBillingGate(r.Context(), proj); err != nil {
		slog.Info("e2b wake refused", "sandbox", id, "project", proj.ID, "err", err)
		httpx.Err(w, http.StatusForbidden, http.StatusForbidden, "this sandbox cannot be resumed: its project is suspended")
		return
	}
	timeout := 300
	if t, err := strconv.Atoi(digAnn(p.Saved, sandbox.AnnotationTimeoutSeconds)); err == nil && t > 0 {
		timeout = t
	}
	if limit := c.ss.Config().WithDefaults().Limits.MaxTimeoutSeconds; limit > 0 && timeout > limit {
		timeout = limit
	}
	ctx, cancel := context.WithTimeout(r.Context(), e2bWakeLimit)
	defer cancel()
	l := slog.With("sandbox", id, "project", p.ProjectID, "port", port)
	if _, err := h.resumePaused(ctx, c, &p, timeout); err != nil {
		status, msg := e2bError(err)
		l.Warn("e2b wake failed", "status", status, "err", err)
		httpx.Err(w, status, status, msg)
		return
	}
	l.Info("e2b wake: resumed")
	httpx.OK(w, map[string]any{"sandboxID": id, "state": "running"})
}

func tokenEq(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func digAnn(obj map[string]any, key string) string {
	md, _ := obj["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	v, _ := ann[key].(string)
	return v
}
