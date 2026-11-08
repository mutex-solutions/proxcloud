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
// Pause = snapshot + save the sandbox's CR (tokens, env, metadata, network) + delete it. Resume (POST
// /sandboxes/{id}/resume, connect to a paused sandbox, or the wake endpoint for autoResume) recreates it under the
// same id from the snapshot image. Processes, memory and open connections are not kept (E2B's mode "filesystem"); a
// request for memory (memory: true, autoPauseMemory: true) is refused with 400, never served as a filesystem pause.
// Fork = snapshot + up to 10 clones with new ids and new tokens. autoPause pauses at the timeout (job
// sandboxAutoPause). A stored snapshot is billed per GB-hour (SANDBOX_SNAPSHOT) until it is resumed or deleted.

import (
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
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	snapshotWait = 15 * time.Minute // how long a background assembly is followed
	maxForkCount = 10

	pausedPausing  = "pausing"
	pausedResuming = "resuming"
	pauseLease     = 10 * time.Minute
	resumeLease    = 20 * time.Minute // covers waiting for a snapshot still being assembled
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
	// State is "" once paused, "pausing" while the sandbox is being removed, "resuming" while a resume brings it back.
	// LeaseUntil bounds a transition: an expired one is a crashed caller's, and the record counts as paused again (or,
	// for a pause, as never paused).
	State      string     `json:"state,omitempty"`
	LeaseUntil *time.Time `json:"leaseUntil,omitempty"`
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
	errSnapshotNotReady = e2bStatusErr{http.StatusConflict, "the snapshot is still being assembled; retry in a few seconds"}
)

func (p *pausedSandbox) leased(now time.Time) bool {
	return p.State != "" && p.LeaseUntil != nil && now.Before(*p.LeaseUntil)
}

// snapshotsOn: this location can take snapshots (the stores are wired, and it has a template registry and a build
// bucket for the assembly).
func (h *Handler) snapshotsOn(c *e2bCaller) error {
	if h.sbTemplates == nil || h.sbPaused == nil || !buildsOn(c.ss.Config()) {
		return errSnapshotsOff
	}
	return nil
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
// assembly (a pod running crane append, tens of seconds) finishes in the background, so pause and create_snapshot
// answer fast and survive the client going away. awaitSnapshot waits for (or completes) it.
// internal = a pause's or fork's snapshot (hidden from GET /snapshots and /templates).
func (h *Handler) snapshotSandbox(ctx context.Context, c *e2bCaller, id, name string, internal bool) (*sandboxTemplateDoc, error) {
	if err := h.snapshotsOn(c); err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx) // a client that gives up must not leave a half-made snapshot
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
			UploadToken: newToken(24), CreatedAt: &now, Kind: "snapshot", Internal: internal, SourceSandbox: id}
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
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(c.ss.SnapshotFS(ctx, c.proj.ID, id, pw)) }()
	if err := bs.put(ctx, key, pr); err != nil {
		_ = pr.CloseWithError(err)
		return undo(fmt.Errorf("snapshot upload: %w", err))
	}
	var size int64
	if head, err := bs.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bs.bucket, Key: &key}); err == nil && head.ContentLength != nil {
		size = *head.ContentLength
	}
	slog.Info("e2b: snapshot captured", "sandbox", id, "template", t.ID, "bytes", size, "took", time.Since(t0))
	layerURL, err := bs.fetchURL(ctx, key)
	if err != nil {
		return undo(err)
	}

	// 2. Record the build, then append the layer onto the sandbox's image in the background.
	if t, err = h.loadTemplate(ctx, c.proj.ID, t.ID); err != nil || t == nil {
		return undo(fmt.Errorf("snapshot record: %v", err))
	}
	t.CPU, t.MemoryGiB, t.GPU, t.SizeBytes = tm.CPU, tm.MemoryGiB, tm.GPU, size
	bl := templateBuild{BuildID: buildID, Status: sandbox.BuildBuilding, StartCmd: tm.StartCmd, ReadyCmd: tm.ReadyCmd,
		StartAsRoot: tm.StartAsRoot, User: tm.User, Workdir: tm.Workdir, From: sandbox.ImageOfSaved(saved),
		Tags: []string{"default"}, CreatedAt: &now}
	t.Builds = append(t.Builds, bl)
	if err := h.saveTemplate(ctx, t); err != nil {
		return undo(err)
	}
	err = c.ss.StartSnapshot(ctx, sandbox.SnapshotRequest{
		ProjectID: t.ProjectID, TemplateID: t.ID, BuildID: buildID, Base: bl.From,
		Image: templateImage(cfg, t, buildID), LayerURL: layerURL,
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
		if _, err := h.awaitSnapshot(bctx, c, t.ID); err != nil {
			slog.Error("e2b: snapshot assembly", "template", t.ID, "err", err)
		}
	}()
	return t, nil
}

func snapshotHash(buildID string) string { return "snapshot-" + buildID }

// failSnapshot records a failed snapshot build.
func (h *Handler) failSnapshot(ctx context.Context, projectID, templateID, buildID string, cause error) {
	_, _, _ = h.finishBuild(ctx, projectID, templateID, buildID, func(_ *sandboxTemplateDoc, b *templateBuild) {
		b.Status, b.Reason = sandbox.BuildError, cause.Error()
	})
}

// awaitSnapshot waits until the snapshot template's latest build is ready (or failed), finishing it from the assembly
// pod's result if nobody has yet. Idempotent: the background goroutine, a waiting resume or create, and the job
// sandboxBuildSync (after an API restart) may all finish it; only the one that records the result bills it.
func (h *Handler) awaitSnapshot(ctx context.Context, c *e2bCaller, templateID string) (*sandboxTemplateDoc, error) {
	for {
		t, err := h.loadTemplate(ctx, c.proj.ID, templateID)
		if err != nil || t == nil || len(t.Builds) == 0 {
			return nil, errors.New("snapshot not found")
		}
		b := &t.Builds[len(t.Builds)-1]
		switch b.Status {
		case sandbox.BuildReady:
			return t, nil
		case sandbox.BuildError:
			return nil, fmt.Errorf("snapshot failed: %s", b.Reason)
		}
		if done, err := h.finishSnapshot(ctx, c, t, b, true); err != nil {
			return nil, err
		} else if done {
			continue
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
	logs := c.ss.BuildLogs(ctx, b.BuildID, maxStoredLogLines)
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
		dctx := context.WithoutCancel(ctx)
		if st.Status == sandbox.BuildReady {
			h.snapshotStorageRow(dctx, c, ft)
		}
		_ = c.ss.DeleteBuild(dctx, b.BuildID)
		if bs, err := buildStorage(c.ss.Config()); err == nil {
			key := contextKey(t.ProjectID, t.ID, snapshotHash(b.BuildID))
			_, _ = bs.client.DeleteObject(dctx, &s3.DeleteObjectInput{Bucket: &bs.bucket, Key: &key})
		}
	}
	return true, nil
}

// snapshotStorageRow bills the stored snapshot by size (SANDBOX_SNAPSHOT, hourly per GB) from the moment it is ready.
// A named snapshot taken again keeps its row and its start; the size follows the newest build.
func (h *Handler) snapshotStorageRow(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc) {
	kind := "snapshot"
	if t.Internal {
		kind = "pause"
	}
	now := time.Now().UTC()
	data := map[string]any{"name": t.Name, "kind": kind, "sizeGB": float64(t.SizeBytes) / (1 << 30), "createdAt": now.Format(time.RFC3339)}
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
	if err := h.deleteTemplate(ctx, c.ss, t); err != nil {
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

// pauseSandbox snapshots, saves and deletes a running sandbox.
func (h *Handler) pauseSandbox(ctx context.Context, c *e2bCaller, id string) error {
	if err := h.snapshotsOn(c); err != nil {
		return err
	}
	saved, err := c.ss.SavedSandbox(ctx, c.proj.ID, id)
	if err != nil {
		return err
	}
	t, err := h.snapshotSandbox(ctx, c, id, "", true)
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx) // past this point a half-done pause would lose the sandbox
	if _, err := c.ss.GetSandbox(ctx, c.proj.ID, id); err != nil {
		h.dropSnapshot(ctx, c, t.ID) // killed while its files were being captured: nothing to pause
		return err
	}
	now := time.Now().UTC()
	lease := now.Add(pauseLease)
	// Claim the id: a resume or kill seeing "pausing" waits or answers 409, instead of starting on the sandbox that is
	// about to be deleted.
	claimed := false
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
		// No record, or an expired pause/resume of this (live) sandbox: take it over.
		if found && cur.TemplateID != t.ID {
			defer h.dropSnapshot(ctx, c, cur.TemplateID)
		}
		claimed = true
		return h.sbPaused.Upsert(tc, id, &pausedSandbox{ID: id, ProjectID: c.proj.ID, ServiceID: c.es.ID, TemplateID: t.ID,
			Saved: saved, PausedAt: &now, State: pausedPausing, LeaseUntil: &lease})
	})
	if err != nil {
		h.dropSnapshot(ctx, c, t.ID)
		return err
	}
	undo := func(cause error) error { // the sandbox keeps running; forget the pause
		if claimed {
			_, _ = h.sbPaused.DeleteByID(ctx, id)
		}
		h.dropSnapshot(ctx, c, t.ID)
		return cause
	}
	if cr := h.e2bRow(ctx, c, id); cr != nil {
		err = h.sandboxDelete(ctx, c.es, c.proj, cr)
	} else {
		err = c.ss.DeleteSandbox(ctx, c.proj.ID, id)
	}
	if err != nil {
		return undo(err)
	}
	waitGone(ctx, c, id, time.Minute)
	return h.updatePaused(ctx, c.proj.ID, id, func(p *pausedSandbox) (bool, error) {
		p.State, p.LeaseUntil = "", nil
		return false, nil
	})
}

// POST /sandboxes/{sandboxID}/pause {memory}
func (h *Handler) e2bPause(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	id := chi.URLParam(r, "sandboxID")
	if _, ok := memoryFlag(w, r); !ok {
		return
	}
	if h.loadPaused(r.Context(), c.proj.ID, id) != nil {
		e2bErr(w, errAlreadyPaused.code, errAlreadyPaused.msg)
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
	shutdown, err := sandbox.ShutdownTimeFor(c.ss.Config(), time.Now(), timeoutSeconds)
	if err != nil {
		return "", e2bStatusErr{http.StatusBadRequest, err.Error()}
	}
	md, _ := saved["metadata"].(map[string]any)
	id, _ := md["name"].(string)
	if ann, ok := md["annotations"].(map[string]any); ok && ann[sandbox.AnnotationAutoPause] == "true" {
		ann[sandbox.AnnotationEndAt] = shutdown.UTC().Format(time.RFC3339)
		shutdown = shutdown.Add(sandbox.AutoPauseGrace)
	}
	q := c.es.SandboxProjectQuota(c.proj.Quota)
	if err := c.ss.ResumeSandbox(ctx, c.proj.ID, saved, image, shutdown, &q); err != nil {
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
		_ = c.ss.DeleteSandbox(context.WithoutCancel(ctx), c.proj.ID, id)
		return "", err
	}
	if err := h.e2bWaitReady(ctx, c, row); err != nil {
		_ = h.sandboxDelete(context.WithoutCancel(ctx), c.es, c.proj, row)
		return "", e2bStatusErr{http.StatusServiceUnavailable, "the sandbox did not start in time"}
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
	// same sandbox; a pause under way is waited for (up to a minute).
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
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		case claimStalePause, claimStaleResume:
			if _, gerr := c.ss.GetSandbox(ctx, c.proj.ID, p.ID); gerr == nil {
				// The crashed pause never removed the sandbox, or the crashed resume already started it: it runs.
				// Finish by dropping the pause.
				dctx := context.WithoutCancel(ctx)
				_ = h.updatePaused(dctx, c.proj.ID, p.ID, func(*pausedSandbox) (bool, error) { return true, nil })
				h.dropSnapshot(dctx, c, p.TemplateID)
				if out == claimStalePause {
					return nil, errAlreadyRunning
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
	release := func(cause error) (map[string]any, error) {
		_ = h.updatePaused(ctx, c.proj.ID, p.ID, func(cur *pausedSandbox) (bool, error) {
			if cur.State == pausedResuming {
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
	id, err := h.startSaved(ctx, c, p.Saved, t.defaultBuild().Image, timeoutSeconds)
	if err != nil {
		return release(err)
	}
	dctx := context.WithoutCancel(ctx) // the sandbox runs: the record must go whatever the client does
	_, _ = h.sbPaused.DeleteByID(dctx, p.ID)
	// The snapshot image stays in the registry (the running sandbox uses it); its template record and its storage
	// billing go.
	h.dropSnapshot(dctx, c, p.TemplateID)
	a, err := c.ss.AccessFor(ctx, c.proj.ID, id)
	if err != nil {
		return nil, err
	}
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
	t, err := h.snapshotSandbox(r.Context(), c, chi.URLParam(r, "sandboxID"), name, false)
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
	ctx := r.Context()
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
	t, err := h.snapshotSandbox(ctx, c, id, "", true)
	if err == nil {
		t, err = h.awaitSnapshot(ctx, c, t.ID)
	}
	if err == nil && t.defaultBuild() == nil {
		err = errSnapshotNotReady
	}
	if err != nil {
		h.e2bSnapshotFail(w, err)
		return
	}
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
				a, err = c.ss.AccessFor(ctx, c.proj.ID, newID)
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
	e2bJSON(w, http.StatusCreated, results)
}

// ---- autoPause -------------------------------------------------------------------------------------------------

// AutoPauseSandboxes pauses every auto-pause sandbox past its end (job sandboxAutoPause, every minute). A sandbox
// whose pause fails runs on to its shutdownTime (end + sandbox.AutoPauseGrace), when the cluster removes it.
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
		if err != nil || !ss.Config().Envd || !buildsOn(ss.Config()) {
			continue
		}
		due, err := ss.DueForAutoPause(ctx)
		if err != nil {
			slog.Warn("autopause: list", "service", es.ID, "err", err)
			continue
		}
		for _, d := range due {
			proj, err := h.svc.repo.FindByID(ctx, d.ProjectID)
			if err != nil || proj == nil {
				continue
			}
			c := &e2bCaller{proj: proj, es: es, ss: ss}
			if err := h.pauseSandbox(ctx, c, d.ID); err != nil {
				slog.Error("autopause: pause", "sandbox", d.ID, "err", err)
				continue
			}
			slog.Info("autopause: paused", "sandbox", d.ID, "project", d.ProjectID)
		}
	}
}

// awaitIfSnapshot lets a create from a snapshot that is still being assembled wait for it (bounded by the request),
// instead of failing with "no ready build".
func (h *Handler) awaitIfSnapshot(ctx context.Context, c *e2bCaller, ref string) {
	if h.sbTemplates == nil {
		return
	}
	name, _ := splitNameTag(ref)
	t, err := h.loadTemplate(ctx, c.proj.ID, name)
	if err != nil || t == nil || t.Kind != "snapshot" || t.defaultBuild() != nil {
		return
	}
	_, _ = h.awaitSnapshot(ctx, c, t.ID)
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
		case cur.State == pausedResuming && cur.leased(now):
			out = claimJoined
			return false, nil
		case cur.State == pausedPausing && cur.leased(now):
			out = claimPausing
			return false, nil
		case cur.State == pausedPausing:
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
			d, err := c.ss.GetSandbox(ctx, c.proj.ID, id)
			if err != nil {
				return nil, err
			}
			a, err := c.ss.AccessFor(ctx, c.proj.ID, id)
			if err != nil {
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

// killPaused removes a paused sandbox: its record, then its snapshot (E2B kill of a paused sandbox). busy while a pause
// or resume holds it.
func (h *Handler) killPaused(ctx context.Context, c *e2bCaller, p *pausedSandbox) error {
	err := h.updatePaused(ctx, c.proj.ID, p.ID, func(cur *pausedSandbox) (bool, error) {
		if cur.leased(time.Now()) {
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
const e2bWakeLimit = 2 * time.Minute

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
	if c.es.ID != p.ServiceID || !tokenEq(bearer, c.ss.Config().ProxyWakeToken) { // the token of the sandbox's own location
		httpx.Err(w, http.StatusUnauthorized, http.StatusUnauthorized, "invalid wake token")
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), e2bWakeLimit)
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
