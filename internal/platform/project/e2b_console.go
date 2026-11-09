package project

// The console uses session principals and explicitly projected contracts. Internal
// E2B adapters reuse admission/lifecycle logic without minting or accepting keys.

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Never embed templateBuild, templateImport, or buildDiagnostics here: stored
// capabilities/provenance and future internal fields must not become public.
type consoleDiagnostics struct {
	Revision        int                  `json:"revision"`
	Stages          []sandbox.BuildStage `json:"stages,omitempty"`
	PodReason       string               `json:"podReason,omitempty"`
	PodMessage      string               `json:"podMessage,omitempty"`
	Warnings        []string             `json:"warnings,omitempty"`
	LogsUnavailable bool                 `json:"logsUnavailable,omitempty"`
	Truncated       bool                 `json:"truncated,omitempty"`
	ArchivedAt      *time.Time           `json:"archivedAt,omitempty"`
	CleanedAt       *time.Time           `json:"cleanedAt,omitempty"`
	LastCaptureAt   *time.Time           `json:"lastCaptureAt,omitempty"`
	TerminalTail    []string             `json:"terminalTail,omitempty"`
	LossReason      string               `json:"lossReason,omitempty"`
}

type consoleBuild struct {
	BuildID        string                 `json:"buildID"`
	Status         string                 `json:"status"`
	Reason         string                 `json:"reason,omitempty"`
	Step           string                 `json:"step,omitempty"`
	Mode           string                 `json:"mode"`
	Source         string                 `json:"source,omitempty"`
	Image          string                 `json:"image,omitempty"`
	Digest         string                 `json:"digest,omitempty"`
	BuildResources sandbox.BuildResources `json:"buildResources"`
	Diagnostics    *consoleDiagnostics    `json:"diagnostics,omitempty"`
	Tags           []string               `json:"tags"`
	StartCmd       string                 `json:"startCmd,omitempty"`
	ReadyCmd       string                 `json:"readyCmd,omitempty"`
	CreatedAt      *time.Time             `json:"createdAt,omitempty"`
	FinishedAt     *time.Time             `json:"finishedAt,omitempty"`
}

type consoleTemplate struct {
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	Aliases       []string            `json:"aliases"` // excludes the canonical name
	Kind          string              `json:"kind"`
	Mode          string              `json:"mode"` // build | import | snapshot
	Source        string              `json:"source,omitempty"`
	Image         string              `json:"image,omitempty"`
	Digest        string              `json:"digest,omitempty"`
	CPU           int                 `json:"cpu"`
	MemoryGiB     int                 `json:"memoryGiB"`
	GPU           *sandbox.GPURequest `json:"gpu,omitempty"`
	SizeBytes     int64               `json:"sizeBytes,omitempty"`
	SourceSandbox string              `json:"sourceSandbox,omitempty"`
	Status        string              `json:"status"`
	Ready         bool                `json:"ready"`
	Builds        []consoleBuild      `json:"builds"` // newest first
	CreatedAt     *time.Time          `json:"createdAt,omitempty"`
	UpdatedAt     *time.Time          `json:"updatedAt,omitempty"`
}

// OCI refs are data, not arbitrary provenance/config. Legacy malformed refs may
// contain credentials; do not echo them even when the rest of the record is safe.
func consoleImageRef(value string) string {
	if value == "" {
		return ""
	}
	if _, err := reference.ParseNormalizedNamed(value); err != nil {
		return ""
	}
	return value
}

func consoleDiagnosticsView(d *buildDiagnostics) *consoleDiagnostics {
	if d == nil {
		return nil
	}
	v := &consoleDiagnostics{Revision: d.Revision, PodReason: sandbox.RedactBuildText(d.PodReason),
		PodMessage: sandbox.RedactBuildText(d.PodMessage), LogsUnavailable: d.LogsUnavailable,
		Truncated: d.Truncated, ArchivedAt: d.ArchivedAt, CleanedAt: d.CleanedAt, LastCaptureAt: d.LastCaptureAt, LossReason: sandbox.RedactBuildText(d.LossReason)}
	tailBytes := 0
	for _, line := range d.TerminalTail {
		line = sandbox.RedactBuildText(line)
		if len(v.TerminalTail) >= 400 || tailBytes+len(line) > 64<<10 {
			break
		}
		v.TerminalTail = append(v.TerminalTail, line)
		tailBytes += len(line)
	}
	for _, stage := range d.Stages {
		stage.Reason, stage.Message = sandbox.RedactBuildText(stage.Reason), sandbox.RedactBuildText(stage.Message)
		v.Stages = append(v.Stages, stage)
	}
	for _, warning := range d.Warnings {
		v.Warnings = append(v.Warnings, sandbox.RedactBuildText(warning))
	}
	return v
}

func consoleBuildView(t *sandboxTemplateDoc, b *templateBuild, tags []string) consoleBuild {
	if tags == nil {
		tags = []string{}
	}
	sort.Strings(tags)
	v := consoleBuild{BuildID: b.BuildID, Status: buildPinSDKStatus(b), Reason: sandbox.RedactBuildText(b.Reason),
		Step: sandbox.RedactBuildText(b.Step), Mode: "build", Source: consoleImageRef(b.From), Image: consoleImageRef(b.Image),
		BuildResources: b.BuildResources, Diagnostics: consoleDiagnosticsView(b.Diagnostics), Tags: tags,
		StartCmd: sandbox.RedactBuildText(b.StartCmd), ReadyCmd: sandbox.RedactBuildText(b.ReadyCmd), CreatedAt: b.CreatedAt, FinishedAt: b.FinishedAt}
	if b.Import != nil {
		v.Mode, v.Source = "import", consoleImageRef(b.Import.Source)
	}
	if t.Kind == "snapshot" {
		v.Mode = "snapshot"
	}
	_, v.Digest, _ = strings.Cut(v.Image, "@")
	return v
}

func consoleView(t *sandboxTemplateDoc) consoleTemplate {
	v := consoleTemplate{ID: t.ID, Name: t.Name, Aliases: append([]string{}, t.Aliases...), Kind: "template", Mode: "build", CPU: t.CPU, MemoryGiB: t.MemoryGiB, GPU: t.GPU,
		SizeBytes: templateStoredSize(t), SourceSandbox: t.SourceSandbox, Status: "waiting", Ready: t.defaultBuild() != nil,
		Builds: []consoleBuild{}, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
	if t.Kind == "snapshot" {
		v.Kind, v.Mode = "snapshot", "snapshot"
	}
	tagsOf := map[string][]string{}
	for tg, id := range t.Tags {
		tagsOf[id] = append(tagsOf[id], tg)
	}
	for i := len(t.Builds) - 1; i >= 0; i-- {
		v.Builds = append(v.Builds, consoleBuildView(t, &t.Builds[i], tagsOf[t.Builds[i].BuildID]))
	}
	if len(v.Builds) > 0 {
		b := v.Builds[0]
		v.Status, v.Mode, v.Source, v.Image, v.Digest = b.Status, b.Mode, b.Source, b.Image, b.Digest
	}
	return v
}

// consoleCaller requires a session and project cloud-resource management rights.
// Unlike API-key administration, template operations do not require a key store.
func (h *Handler) consoleCaller(w http.ResponseWriter, r *http.Request) (*e2bCaller, bool) {
	if rc := httpx.RC(r.Context()); rc == nil || rc.Sub == "" {
		httpx.Err(w, http.StatusUnauthorized, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	if h.sbTemplates == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "sandbox templates are not configured")
		return nil, false
	}
	u, ok := h.principal(w, r)
	if !ok {
		return nil, false
	}
	p, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return nil, false
	}
	c, status, msg := h.e2bCallerFor(r.Context(), u, p)
	if c == nil {
		httpx.Err(w, status, status, msg)
		return nil, false
	}
	return c, true
}

func (h *Handler) consoleTemplateList(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r)
	if !ok {
		return
	}
	out := []consoleTemplate{}
	for _, t := range h.projectTemplates(r.Context(), c.proj.ID) {
		out = append(out, consoleView(&t))
	}
	httpx.OK(w, out)
}

// consoleTemplate resolves only visible records in the authorized project. Never
// let internal pause/fork snapshots cross the console adapter boundary.
func (h *Handler) consoleTemplate(w http.ResponseWriter, r *http.Request, c *e2bCaller) *sandboxTemplateDoc {
	t, err := h.loadTemplate(r.Context(), c.proj.ID, chi.URLParam(r, "templateId"))
	if err != nil || t == nil || t.Internal {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "template not found")
		return nil
	}
	return t
}

// Logs/status includes waiting, active and finalized builds. Offset is a stable
// index into the immutable archived prefix, including terminal diagnostics.
func (h *Handler) consoleTemplateLogs(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r)
	if !ok {
		return
	}
	t := h.consoleTemplate(w, r, c)
	if t == nil {
		return
	}
	bl := t.build(chi.URLParam(r, "buildId"))
	if bl == nil {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "build not found")
		return
	}
	offset := 0
	if value := r.URL.Query().Get("logsOffset"); value != "" {
		var err error
		offset, err = strconv.Atoi(value)
		if err != nil || offset < 0 {
			httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "logsOffset must be a non-negative integer")
			return
		}
	}
	if (bl.Registry != nil || buildNeedsSync(bl)) && (t.Kind != "snapshot" || buildTerminal(bl)) {
		h.syncBuild(r.Context(), c, t, bl)
	}
	logs := []string{}
	for _, line := range buildLogPage(bl, offset) {
		logs = append(logs, sandbox.RedactBuildText(line))
	}
	view := consoleBuildView(t, bl, nil)
	httpx.OK(w, map[string]any{"templateID": t.ID, "buildID": bl.BuildID, "status": view.Status, "reason": view.Reason, "step": view.Step,
		"logs": logs, "nextLogsOffset": len(bl.Logs), "buildResources": view.BuildResources, "diagnostics": view.Diagnostics})
}

func (h *Handler) consoleTemplateDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r)
	if !ok {
		return
	}
	if h.consoleTemplate(w, r, c) == nil {
		return
	}
	// Preserve alias deletion semantics; canonical/id deletion uses the same
	// project lock and active-worker refusal as the API-key surface.
	request := consoleE2BRequest(r, c, chi.URLParam(r, "templateId"), "", nil)
	writeConsoleE2B(w, captureConsoleE2B(request, func(w http.ResponseWriter, request *http.Request) {
		h.e2bTemplateDeleteLocked(w, request)
	}))
}

func templateStoredSize(t *sandboxTemplateDoc) int64 {
	var size int64
	for _, b := range t.Builds {
		if b.Status == sandbox.BuildReady {
			size += b.SizeBytes
		}
	}
	return size
}
