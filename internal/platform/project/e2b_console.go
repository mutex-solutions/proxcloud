package project

// e2b_console.go — the client console's Templates tab: the project's custom templates (the same records the E2B API
// manages), their build logs, and delete. Building stays with the E2B SDKs and CLI (docs/managed-sandbox.md §7).

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type consoleBuild struct {
	BuildID    string     `json:"buildID"`
	Status     string     `json:"status"`
	Reason     string     `json:"reason,omitempty"`
	Tags       []string   `json:"tags"`
	From       string     `json:"from,omitempty"`
	StartCmd   string     `json:"startCmd,omitempty"`
	ReadyCmd   string     `json:"readyCmd,omitempty"`
	CreatedAt  *time.Time `json:"createdAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type consoleTemplate struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	CPU       int            `json:"cpu"`
	MemoryGiB int            `json:"memoryGiB"`
	Status    string         `json:"status"` // the latest build's
	Ready     bool           `json:"ready"`  // a sandbox can start from it
	Builds    []consoleBuild `json:"builds"` // newest first
	CreatedAt *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt *time.Time     `json:"updatedAt,omitempty"`
}

func consoleView(t *sandboxTemplateDoc) consoleTemplate {
	v := consoleTemplate{ID: t.ID, Name: t.Name, CPU: t.CPU, MemoryGiB: t.MemoryGiB, Status: "waiting",
		Ready: t.defaultBuild() != nil, Builds: []consoleBuild{}, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
	tagsOf := map[string][]string{}
	for tg, id := range t.Tags {
		tagsOf[id] = append(tagsOf[id], tg)
	}
	for i := len(t.Builds) - 1; i >= 0; i-- {
		b := t.Builds[i]
		tags := tagsOf[b.BuildID]
		if tags == nil {
			tags = []string{}
		}
		v.Builds = append(v.Builds, consoleBuild{BuildID: b.BuildID, Status: buildPinSDKStatus(&b), Reason: b.Reason, Tags: tags, From: b.From,
			StartCmd: b.StartCmd, ReadyCmd: b.ReadyCmd, CreatedAt: b.CreatedAt, FinishedAt: b.FinishedAt})
	}
	if len(v.Builds) > 0 {
		v.Status = v.Builds[0].Status
	}
	return v
}

// consoleCaller authorizes the session user on the project (perm) and resolves its sandbox provider.
func (h *Handler) consoleCaller(w http.ResponseWriter, r *http.Request, perm string) (*e2bCaller, bool) {
	if h.sbTemplates == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "sandbox templates are not configured")
		return nil, false
	}
	u, ok := h.principal(w, r)
	if !ok {
		return nil, false
	}
	p, ok := h.project(w, r, u, perm)
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

// GET /project/{id}/sandbox-templates
func (h *Handler) consoleTemplateList(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r, rbac.ProjectCloudResourceRead)
	if !ok {
		return
	}
	out := []consoleTemplate{}
	for _, t := range h.projectTemplates(r.Context(), c.proj.ID) {
		out = append(out, consoleView(&t))
	}
	httpx.OK(w, out)
}

// GET /project/{id}/sandbox-templates/{templateId}/builds/{buildId}/logs
func (h *Handler) consoleTemplateLogs(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r, rbac.ProjectCloudResourceRead)
	if !ok {
		return
	}
	ctx := r.Context()
	t, err := h.loadTemplate(ctx, c.proj.ID, chi.URLParam(r, "templateId"))
	if err != nil || t == nil || t.Internal {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "template not found")
		return
	}
	bl := t.build(chi.URLParam(r, "buildId"))
	if bl == nil {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "build not found")
		return
	}
	logs := bl.Logs
	if bl.Registry != nil || bl.Status == sandbox.BuildBuilding {
		logs = h.syncBuildPod(ctx, c, t, bl, true)
	}
	if logs == nil {
		logs = []string{}
	}
	httpx.OK(w, map[string]any{"status": buildPinSDKStatus(bl), "reason": bl.Reason, "logs": logs})
}

// DELETE /project/{id}/sandbox-templates/{templateId}
func (h *Handler) consoleTemplateDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	t, err := h.loadTemplate(r.Context(), c.proj.ID, chi.URLParam(r, "templateId"))
	if err != nil || t == nil || t.Internal {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "template not found")
		return
	}
	if err := h.deleteTemplate(r.Context(), c.ss, t); err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{})
}
