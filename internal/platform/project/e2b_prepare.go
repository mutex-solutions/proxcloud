package project

import (
	"context"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"log/slog"
	"net/http"
)

// ProxCloud extension, never a 202 replacement for E2B create. POST admits/refreshes finite warm
// intent; GET reports current N/M coverage. The client may poll GET before stock SDK create.
func (h *Handler) e2bPrepareTemplate(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	ref := chi.URLParam(r, "templateID")
	t, _, err := h.resolveCustomTemplate(r.Context(), c.proj.ID, ref)
	if err != nil {
		if errors.Is(err, sandbox.ErrBadTemplate) {
			e2bErr(w, http.StatusConflict, "template has no ready artifact; wait for its build/import status before preparing")
		} else {
			e2bFail(w, err)
		}
		return
	}
	if t == nil {
		e2bErr(w, http.StatusBadRequest, "prepare requires a ready project template; built-in warming is operator managed")
		return
	}
	var coverage *sandbox.WarmCoverage
	if r.Method == http.MethodPost {
		coverage, err = c.ss.PrepareImage(r.Context(), c.proj.ID, *t, 0)
	} else {
		coverage, err = c.ss.ImageWarmCoverage(r.Context(), c.proj.ID, *t)
	}
	if err != nil {
		// Admission errors are public, but cluster transport/RBAC/lease internals are not.
		if errors.Is(err, sandbox.ErrBadBuild) {
			e2bErr(w, http.StatusBadRequest, err.Error())
		} else {
			slog.Warn("template preparation unavailable", "err", err)
			e2bErr(w, http.StatusServiceUnavailable, "template preparation unavailable or busy; retry shortly")
		}
		return
	}
	e2bJSON(w, http.StatusOK, coverage)
}

// warmTemplate is safe to repeat after build/import completion. Errors must not turn a durable
// artifact into a failed build: warming and workload readiness are deliberately independent.
func (h *Handler) warmTemplate(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, bl *templateBuild) {
	if bl == nil || bl.Status != sandbox.BuildReady || !c.ss.Config().Warming.Enabled {
		return
	}
	tpl := sandbox.Template{Image: bl.Image, CPU: t.CPU, MemoryGiB: t.MemoryGiB, GPU: t.GPU}
	var sizeBytes int64
	if bl.Import != nil && bl.Import.Result != nil {
		sizeBytes = bl.Import.Result.SizeBytes
	}
	if _, err := c.ss.PrepareImage(ctx, t.ProjectID, tpl, sizeBytes); err != nil {
		slog.Warn("template warming admission failed", "template", t.ID, "err", err)
	}
}
