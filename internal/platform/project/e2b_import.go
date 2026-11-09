package project

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

type templateImport struct {
	Source      string                   `json:"source"`
	Destination string                   `json:"destination"`
	Key         string                   `json:"key"`
	Result      *sandboximport.Result    `json:"result,omitempty"`
	Registry    *importRegistryOperation `json:"registryOperation,omitempty"`
}

type importTemplateRequest struct {
	Source   string   `json:"source"`
	Name     string   `json:"name"`
	Aliases  []string `json:"aliases,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	CPUCount int      `json:"cpuCount,omitempty"`
	MemoryMB int      `json:"memoryMB,omitempty"`
	User     string   `json:"user,omitempty"`
	Workdir  string   `json:"workdir,omitempty"`
	StartCmd string   `json:"startCmd,omitempty"`
	ReadyCmd string   `json:"readyCmd,omitempty"`
	Registry *struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"registry,omitempty"`
}

func (t *sandboxTemplateDoc) names() []string   { return append([]string{t.Name}, t.Aliases...) }
func projectTemplateLock(project string) string { return "sandbox-template-names:" + project }

// Successful builds are durable artifact roots, not disposable logs. Until an explicit
// reference-aware history deletion API exists, keep ALL successes, tagged builds and active
// work; bound only unreferenced failed history. This deliberately prefers storage over data loss.
func retainTemplateBuilds(t *sandboxTemplateDoc) {
	trimBuilds(t)
}

func importKey(b importTemplateRequest, cpu, mem int) string {
	// Aliases/tags and credentials are NOT part of settings or persisted provenance.
	raw, _ := json.Marshal([]any{b.Source, cpu, mem, b.User, b.Workdir, b.StartCmd, b.ReadyCmd})
	s := sha256.Sum256(raw)
	return hex.EncodeToString(s[:])
}
func validateImportRequest(b *importTemplateRequest, project string, cfg sandbox.Config) error {
	policy := sandboximport.Policy{TemplateRegistry: cfg.TemplateRegistry, SourceRepositories: cfg.Import.SourceRepositories, AllowAnonymous: cfg.Import.AllowAnonymous}
	canonical, err := policy.Source(b.Source, project)
	if err != nil {
		return err
	}
	if canonical && b.Registry != nil {
		return errors.New("managed project images use runtime pull verification; do not supply registry credentials")
	}
	if !canonical && b.Registry == nil && !cfg.Import.AllowAnonymous {
		return errors.New("approved external sources require explicit registry credentials")
	}
	if b.Registry != nil && (b.Registry.Username == "" || b.Registry.Password == "") {
		return errors.New("registry.username and registry.password must both be supplied; anonymous access requires the operator allowAnonymous policy")
	}
	if b.Registry != nil && (len(b.Registry.Username) > 1024 || len(b.Registry.Password) > 32<<10 || strings.ContainsAny(b.Registry.Username, ":\x00\r\n")) {
		return errors.New("invalid registry credentials")
	}
	if len(b.Aliases) > 19 || len(b.Tags) > 64 {
		return errors.New("at most 19 aliases and 64 tags")
	}
	for _, n := range append(append([]string{b.Name}, b.Aliases...), b.Tags...) {
		if !templateTagRe.MatchString(n) {
			return errors.New("name, aliases and tags must be 1-63 lowercase letters, digits, '.', '_' or '-'")
		}
	}
	for _, n := range append([]string{b.Name}, b.Aliases...) {
		if _, ok := cfg.Templates[n]; ok {
			return errors.New("name or alias collides with a built-in template")
		}
	}
	if b.CPUCount < 0 || b.MemoryMB < 0 {
		return errors.New("runtime size must be positive when specified")
	}
	cpu, mem := templateSize(b.CPUCount, b.MemoryMB)
	lim := cfg.WithDefaults().Limits
	if cpu > lim.MaxCPU || mem > lim.MaxMemoryGiB {
		return errors.New("runtime size exceeds the location limits")
	}
	if err = sandboximport.ValidateRuntime(b.User, b.Workdir); err != nil {
		return err
	}
	if len(b.StartCmd) > 32<<10 || len(b.ReadyCmd) > 32<<10 || strings.ContainsRune(b.StartCmd, '\x00') || strings.ContainsRune(b.ReadyCmd, '\x00') {
		return errors.New("startCmd and readyCmd must be at most 32768 bytes and contain no NUL")
	}
	return nil
}

// POST /templates/import: ProxCloud extension under the SAME project API-key middleware.
func (h *Handler) e2bTemplateImport(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	var b importTemplateRequest
	if !decodeE2B(w, r, &b) {
		return
	}
	if err := validateImportRequest(&b, c.proj.ID, c.ss.Config()); err != nil {
		e2bErr(w, 400, err.Error())
		return
	}
	if h.sbBuildPinsDB == nil {
		e2bErr(w, 503, "digest import requires durable registry protection")
		return
	}
	if err := h.sandboxUsageGate(r.Context(), c.proj, c.es); err != nil {
		e2bFail(w, err)
		return
	}
	if _, err := c.ss.ImportWorker(r.Context()); err != nil {
		e2bErr(w, 503, "digest import is not enabled on this location (trusted worker required)")
		return
	}
	active, err := c.ss.ActiveBuildWorkers(r.Context(), c.proj.ID)
	if err != nil {
		e2bFail(w, err)
		return
	}
	var t *sandboxTemplateDoc
	var bl *templateBuild
	created := false
	err = h.sbTemplates.DB().WithTx(r.Context(), func(ctx context.Context) error {
		if err := h.sbTemplates.DB().LockXact(ctx, projectTemplateLock(c.proj.ID)); err != nil {
			return err
		}
		if err := h.sbTemplates.DB().LockXact(ctx, "sandbox-registry-references"); err != nil {
			return err
		}
		var all []sandboxTemplateDoc
		if err := h.sbTemplates.Find(ctx, pgdoc.M{"projectId": c.proj.ID}, &all); err != nil {
			return err
		}
		cpu, mem := templateSize(b.CPUCount, b.MemoryMB)
		key := importKey(b, cpu, mem)
		// Dedup strictly within the authorized project and exact source/settings. Never consult another tenant.
		for i := range all {
			v := &all[i]
			if v.Internal || v.Deleting {
				continue
			}
			for j := range v.Builds {
				bb := &v.Builds[j]
				if bb.Import != nil && bb.Import.Key == key && v.CPU == cpu && v.MemoryGiB == mem {
					t = v
					bl = bb
					break
				}
			}
			if t != nil {
				break
			}
		}
		if t != nil {
			var locked sandboxTemplateDoc
			found, err := h.sbTemplates.GetForUpdate(ctx, t.ID, &locked)
			if err != nil {
				return err
			}
			if !found {
				return sandbox.ErrNotFound
			}
			id := bl.BuildID
			t = &locked
			bl = t.build(id)
			if bl == nil {
				return sandbox.ErrNotFound
			}
		}
		names := append([]string{b.Name}, b.Aliases...)
		for _, v := range all {
			for _, n := range names {
				if (slices.Contains(v.names(), n) || v.ID == n) && (t == nil || v.ID != t.ID) {
					return errImportConflict
				}
			}
		}
		now := time.Now().UTC()
		if t == nil {
			count := 0
			for _, v := range all {
				if !v.Internal {
					count++
				}
			}
			if count >= maxTemplatesPerProject {
				return sandbox.ErrQuotaExceeded
			}
			if err := h.workerAdmission(ctx, c, "", active); err != nil {
				return err
			}
			id := newTemplateID()
			canonical, _ := c.ss.ValidateImportSource(b.Source, c.proj.ID, b.Registry != nil || c.ss.Config().Import.AllowAnonymous)
			buildID := uuid.NewString()
			dest := strings.TrimRight(c.ss.Config().TemplateRegistry, "/") + "/" + c.proj.ID + "/" + id + ":" + buildID
			if canonical {
				dest = strings.Split(b.Source, "@")[0]
			}
			t = &sandboxTemplateDoc{ID: id, ProjectID: c.proj.ID, Name: b.Name, CPU: cpu, MemoryGiB: mem, Tags: map[string]string{}, CreatedAt: &now, UpdatedAt: &now}
			bb := templateBuild{BuildID: buildID, Status: sandbox.BuildWaiting, From: b.Source, User: b.User, Workdir: b.Workdir, StartCmd: b.StartCmd, ReadyCmd: b.ReadyCmd, CreatedAt: &now, Import: &templateImport{Source: b.Source, Destination: dest, Key: key, Registry: &importRegistryOperation{Operation: registrypins.Operation{ID: uuid.NewString(), ProjectID: c.proj.ID, Owner: "import:" + id + ":" + buildID, Generation: uuid.NewString()}, State: buildPreparing}}}
			t.Builds = []templateBuild{bb}
			bl = &t.Builds[0]
			created = true
		}
		for _, n := range names {
			if n != t.Name && !slices.Contains(t.Aliases, n) {
				t.Aliases = append(t.Aliases, n)
			}
		}
		if len(t.Aliases) > 19 {
			return errImportConflict
		}
		tags := b.Tags
		if len(tags) == 0 {
			tags = []string{"default"}
		}
		for _, tg := range tags {
			if !slices.Contains(bl.Tags, tg) {
				bl.Tags = append(bl.Tags, tg)
			}
			if bl.Status == sandbox.BuildReady {
				t.Tags[tg] = bl.BuildID
			}
		}
		t.UpdatedAt = &now
		if created {
			return h.sbTemplates.Upsert(ctx, t.ID, t)
		}
		_, err := h.sbTemplates.Replace(ctx, t.ID, t)
		return err
	})
	if err != nil {
		switch {
		case errors.Is(err, errImportConflict):
			e2bErr(w, 409, "name or alias already identifies a different template/settings")
		case errors.Is(err, sandbox.ErrQuotaExceeded):
			e2bErr(w, 429, "template or concurrent import/build quota exceeded")
		default:
			e2bFail(w, err)
		}
		return
	}
	if created {
		dc := []byte(`{"auths":{}}`)
		if b.Registry != nil {
			host, _, _ := strings.Cut(b.Source, "/")
			dc, _ = json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(b.Registry.Username + ":" + b.Registry.Password))}}})
		}
		store, proof, err := h.acquireImportDispatch(r.Context(), c, t, bl)
		if err != nil {
			e2bFail(w, err)
			return
		}
		launch, cancel := context.WithTimeout(r.Context(), buildLaunchTimeout)
		err = h.withSandboxStart(launch, c.es.ID, func(ctx context.Context) error {
			return c.ss.StartImport(ctx, sandbox.ImportRequest{ProjectID: t.ProjectID, TemplateID: t.ID, BuildID: bl.BuildID, Source: b.Source, Destination: bl.Import.Destination, DockerConfig: dc, Pins: proof})
		})
		if err == nil {
			err = launch.Err()
		}
		cancel()
		if err != nil {
			h.compensateImportLaunch(r.Context(), store, proof, t, bl)
			e2bErr(w, 503, "trusted import worker outcome could not be confirmed")
			return
		}
	}
	e2bJSON(w, http.StatusAccepted, map[string]any{"templateID": t.ID, "buildID": bl.BuildID, "status": buildPinSDKStatus(bl), "aliases": t.names(), "reused": !created})
}

// workerAdmission must run under projectTemplateLock. Union durable in-flight
// reservations with live workers so a reserve-before-pod snapshot/import is visible
// and a surviving pod after an uncertain write still consumes its slot. Ordinary
// waiting v3 contexts are not started workers and must not exhaust this quota.
func (h *Handler) workerAdmission(ctx context.Context, c *e2bCaller, excludeBuildID string, observed map[string]bool) error {
	active := make(map[string]bool, len(observed))
	for id, live := range observed {
		active[id] = live
	}
	var all []sandboxTemplateDoc
	if err := h.sbTemplates.Find(ctx, pgdoc.M{"projectId": c.proj.ID}, &all); err != nil {
		return err
	}
	for _, t := range all {
		for _, b := range t.Builds {
			if buildPinActive(b) || importPinActive(b) || b.CancelRequested || b.Status == sandbox.BuildBuilding || b.Status == sandbox.BuildWaiting && b.Import != nil {
				active[b.BuildID] = true
			}
		}
	}
	delete(active, excludeBuildID)
	if len(active) >= c.ss.Config().WithDefaults().Build.MaxConcurrent {
		return sandbox.ErrQuotaExceeded
	}
	return nil
}

var errImportConflict = errors.New("import alias conflict")

func (h *Handler) syncImport(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, b *templateBuild) []string {
	op := b.Import.Registry
	if op == nil || op.State == buildPinUnknown || op.State == buildPreparing || op.State == buildPinned {
		return b.Logs
	}
	if op.State != buildPinFinished {
		status, result, reason, err := c.ss.ImportState(ctx, t.ProjectID, b.BuildID)
		if op.State == buildTerminalObserved {
			status, result, reason = b.Status, b.Import.Result, b.Reason
			err = nil
		}
		if err != nil || status != sandbox.BuildReady && status != sandbox.BuildError {
			// A timeout or absent worker is ambiguous, never known no-effect.
			if b.CreatedAt != nil && time.Since(*b.CreatedAt) > sandbox.BuildTimeout+5*time.Minute {
				if store, err := h.buildPinStore(c.ss.Config()); err == nil && store != nil {
					h.markImportUnknown(ctx, store, t, b)
				}
			}
			return b.Logs
		}
		if err := h.finishImportPins(ctx, c, t, b, status, result, reason); err != nil {
			return b.Logs
		}
	}
	if b.Import.Registry.State == buildPinFinished {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = c.ss.DeleteImport(cleanup, b.BuildID)
		cancel()
		h.warmTemplate(ctx, c, t, b)
	}
	return b.Logs
}

func (h *Handler) e2bTemplateAliases(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	var b struct {
		Aliases []string `json:"aliases"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	for _, a := range b.Aliases {
		if !templateTagRe.MatchString(a) {
			e2bErr(w, 400, "invalid alias")
			return
		}
		if _, ok := c.ss.Config().Templates[a]; ok {
			e2bErr(w, 409, "alias collides with a built-in template")
			return
		}
	}
	var out []string
	err := h.sbTemplates.DB().WithTx(r.Context(), func(ctx context.Context) error {
		if err := h.sbTemplates.DB().LockXact(ctx, projectTemplateLock(c.proj.ID)); err != nil {
			return err
		}
		t, err := h.loadTemplate(ctx, c.proj.ID, chi.URLParam(r, "templateID"))
		if err != nil {
			return err
		}
		if t == nil || t.Internal || t.Deleting {
			return sandbox.ErrNotFound
		}
		var locked sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(ctx, t.ID, &locked)
		if err != nil {
			return err
		}
		if !found {
			return sandbox.ErrNotFound
		}
		t = &locked
		for _, a := range b.Aliases {
			if r.Method == http.MethodDelete {
				t.Aliases = slices.DeleteFunc(t.Aliases, func(n string) bool { return n == a })
				continue
			}
			other, err := h.loadTemplate(ctx, c.proj.ID, a)
			if err != nil {
				return err
			}
			if other != nil && other.ID != t.ID {
				return errImportConflict
			}
			if a != t.Name && !slices.Contains(t.Aliases, a) {
				t.Aliases = append(t.Aliases, a)
			}
		}
		if len(t.Aliases) > 19 {
			return errImportConflict
		}
		out = t.names()
		_, err = h.sbTemplates.Replace(ctx, t.ID, t)
		return err
	})
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			e2bErr(w, 404, "template not found")
		} else if errors.Is(err, errImportConflict) {
			e2bErr(w, 409, "alias collision or alias limit exceeded")
		} else {
			e2bFail(w, err)
		}
		return
	}
	e2bJSON(w, 200, map[string]any{"aliases": out})
}

// Serialize ordinary create/build/delete with alias/import reservations too, including absent
// rows. The build handler's active-worker check and pod creation must not race template deletion.
func (h *Handler) e2bTemplateRequestLocked(w http.ResponseWriter, r *http.Request) {
	h.withTemplateNameLock(w, r, h.e2bTemplateRequest)
}
func (h *Handler) e2bTemplateBuildLocked(w http.ResponseWriter, r *http.Request) {
	// Build admission commits a durable reservation before touching Kubernetes; an
	// encompassing transaction would hide the fetch token until after pod creation.
	h.e2bTemplateBuild(w, r)
}
func (h *Handler) e2bTemplateDeleteLocked(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	t, err := h.loadTemplate(r.Context(), c.proj.ID, chi.URLParam(r, "templateID"))
	if err != nil || t == nil || t.Internal {
		e2bErr(w, 404, "template not found")
		return
	}
	if name := chi.URLParam(r, "templateID"); name != t.ID && name != t.Name {
		raw, _ := json.Marshal(map[string]any{"aliases": []string{name}})
		req := consoleE2BRequest(r, c, t.ID, "", raw)
		req.Method = http.MethodDelete
		h.e2bTemplateAliases(w, req)
		return
	}
	h.e2bTemplateDelete(w, r)
}
func (h *Handler) withTemplateNameLock(w http.ResponseWriter, r *http.Request, handler http.HandlerFunc) {
	if !h.templatesOn(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), templateMutationTimeout)
	defer cancel()
	// Never publish a successful response until the transaction (including COMMIT)
	// succeeds. In particular, canceled requests must not acknowledge rolled-back work.
	response := newBufferedResponse()
	err := h.sbTemplates.DB().WithTx(ctx, func(ctx context.Context) error {
		if err := h.sbTemplates.DB().LockXact(ctx, projectTemplateLock(e2bFrom(r).proj.ID)); err != nil {
			return err
		}
		handler(response, r.WithContext(ctx))
		return ctx.Err()
	})
	if err != nil {
		e2bFail(w, err)
		return
	}
	response.publish(w)
}
