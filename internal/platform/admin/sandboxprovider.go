package admin

// sandboxprovider.go — the operator side of a sandbox provider (docs/managed-sandbox.md §3). A sandbox provider may be
// stored DISABLED at any time; it goes live (any other status) only when its release gates hold, on create and on every
// update that leaves it live:
//
//	POST /service/{id}/sandbox/release-check → [{name, ok, detail}] for the three gates, stored nothing

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// SetSandboxGates wires the release-gate check (syncjob.Job.SandboxReleaseGates in cmd/api). nil: no sandbox provider can
// go live.
func (h *Handler) SetSandboxGates(f func(ctx context.Context, es *externalservice.ExternalService) []sandbox.Gate) {
	h.sandboxGates = f
}

func (h *Handler) routeSandboxProvider(r chi.Router) {
	r.Post("/service/{id}/sandbox/release-check", h.sandboxReleaseCheck)
	r.Get("/service/{id}/sandbox/rotation-plan", h.sandboxRotationPlan)
	r.Get("/sandbox/abuse", h.sandboxAbuseList)
}

// sandboxOf reads a stored doc as a sandbox provider with its secret decrypted; nil for any other provider.
func (h *Handler) sandboxOf(doc pgdoc.M) *externalservice.ExternalService {
	cfg, _ := doc["config"].(map[string]any)
	id, _ := doc["_id"].(string)
	status, _ := doc["status"].(string)
	es := &externalservice.ExternalService{ID: id, Status: status, Config: cfg, Secret: doc["secret"]}
	if !es.IsSandbox() {
		return nil
	}
	if h.esSvc != nil {
		h.esSvc.Decrypt(es)
	}
	return es
}

// sandboxGatesFor runs the gates on a sandbox provider doc (fail closed without a checker).
func (h *Handler) sandboxGatesFor(ctx context.Context, es *externalservice.ExternalService) []sandbox.Gate {
	if h.sandboxGates == nil {
		return []sandbox.Gate{{Name: "checker", Detail: "no release-gate checker is wired in this API"}}
	}
	if err := es.SandboxConfig().Validate(); err != nil {
		return []sandbox.Gate{{Name: "config", Detail: err.Error()}}
	}
	return h.sandboxGates(ctx, es)
}

// checkSandboxGoLive refuses a sandbox provider doc that would be live while a release gate does not hold. A DISABLED
// one, and any other provider, passes.
func (h *Handler) checkSandboxGoLive(ctx context.Context, doc pgdoc.M) *httpx.HTTPError {
	es := h.sandboxOf(doc)
	if es == nil {
		return nil
	}
	if err := es.SandboxConfig().ValidateAutomaticActions(); err != nil {
		return httpx.BadRequest(err.Error())
	}
	if herr := refuseFunctionsEnabled(es); herr != nil {
		return herr
	}
	if doc["status"] == externalservice.StatusDisabled {
		return nil
	}
	// Both reasons in one answer. With the scheduler off the refusal used to name the scheduler alone, so an operator
	// who then turned it on met the gates one save later (lab, 2026-11-26); the gates are read either way.
	herr, err := h.sandboxNeedsScheduler(doc), sandbox.GatesErr(h.sandboxGatesFor(ctx, es))
	switch {
	case herr != nil && err != nil:
		herr.Msg += " Besides the scheduler: " + err.Error() + ". The provider stays DISABLED until they hold too (docs/managed-sandbox.md §3)."
		return herr
	case herr != nil:
		herr.Msg += " No release gate fails at this moment."
		return herr
	case err != nil:
		return httpx.NewError(http.StatusConflict, http.StatusConflict,
			"A sandbox provider cannot go live: "+err.Error()+". Store it with status DISABLED until they hold (docs/managed-sandbox.md §3).")
	}
	return nil
}

// sandboxReleaseCheck handles POST /service/{id}/sandbox/release-check: each gate's answer for the stored provider.
func (h *Handler) sandboxReleaseCheck(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	doc, err := h.repo.FindDoc(r.Context(), externalServiceCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return
	}
	es := h.sandboxOf(doc)
	if es == nil {
		httpx.WriteError(w, httpx.BadRequest("This provider is not a sandbox provider"))
		return
	}
	httpx.OK(w, h.sandboxGatesFor(r.Context(), es))
}

// refuseFunctionsEnabled refuses a sandbox provider doc whose functions block is switched on, whatever its status.
// Nothing can open it yet: enabling needs proof that the platform can stop and bill what it starts (a running
// scheduler, an encryption key, a sandbox-proxy that reports to this portal), and the check of that proof is not part
// of this portal. The rest of the block (limits, the default quota) is stored as written.
func refuseFunctionsEnabled(es *externalservice.ExternalService) *httpx.HTTPError {
	if !es.SandboxConfig().Functions.Enabled {
		return nil
	}
	return httpx.NewError(http.StatusConflict, http.StatusConflict,
		"Functions cannot be enabled on a sandbox provider in this release: store the provider with config.sandbox.functions.enabled false.")
}
