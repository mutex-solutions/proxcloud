package admin

// sandboxprovider.go — the operator side of a sandbox provider (docs/managed-sandbox.md §3). A sandbox provider may be
// stored DISABLED at any time; it goes live (any other status) only when its release gates hold, on create and on every
// update that leaves it live:
//
//	POST /service/{id}/sandbox/release-check → [{name, ok, detail}] for the three gates, stored nothing
//
// The functions block of a sandbox provider (config.sandbox.functions) has a gate of its own (checkFunctionsEnabled):
// enabled is accepted only while the function rows hold. Where the function side exists the release check lists
// those rows after the three gates; they never decide whether the provider goes live for sandboxes.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	if herr := h.checkFunctionsEnabled(ctx, es); herr != nil {
		return herr
	}
	if doc["status"] == externalservice.StatusDisabled {
		return nil
	}
	if herr := h.sandboxNeedsScheduler(doc); herr != nil {
		return herr
	}
	if err := sandbox.GatesErr(h.sandboxGatesFor(ctx, es)); err != nil {
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
	gates := h.sandboxGatesFor(r.Context(), es)
	// The function rows, only where the function side exists: the provider holds a function wake token (a platform
	// run built it), or its block is on.
	if cfg := es.SandboxConfig(); cfg.FunctionWakeToken != "" || cfg.Functions.Enabled {
		gates = append(gates, h.functionGates(r.Context(), es)...)
	}
	httpx.OK(w, gates)
}

// FunctionGate is what the portal knows, outside this handler, of whether functions can be switched on
// (SetFunctionGate).
type FunctionGate struct {
	// SealingKey: the portal has an encryption key, so function secrets and registry passwords are stored sealed.
	SealingKey bool
	// ExternalBilling: usage is rated by an outside billing service (PROXCLOUD_BILLING_PROVIDER=external). It is
	// not sent a function's running intervals, and would bill every idle gap as running.
	ExternalBilling bool
	// Proxies is how many sandbox-proxy replicas of the provider sent this portal an activity report within
	// ProxyWindow, with the provider's function wake token (project.Handler.FunctionProxiesReporting), and
	// ProxyWindow that age as the rows print it (project.FunctionProxyWindow: the one constant the count, the
	// records' keep and this text share).
	Proxies     func(ctx context.Context, serviceID string) (int, error)
	ProxyWindow time.Duration
}

// SetFunctionGate wires the inputs of the function gate (cmd/api).
func (h *Handler) SetFunctionGate(g FunctionGate) { h.functionGate = g }

// The function rows of the release check. Their names start with "function": they decide whether
// config.sandbox.functions.enabled is accepted, never whether the provider goes live for sandboxes.
const (
	gateFunctionScheduler = "function-scheduler"
	gateFunctionKey       = "function-sealing-key"
	gateFunctionProxy     = "function-proxy"
	gateFunctionBilling   = "function-billing"
	// gateFunctionPrice warns and refuses nothing.
	gateFunctionPrice = "function-price"
)

// functionGates are the rows that decide whether a sandbox provider's functions block may be on, and the price
// warning:
//
//   - the job scheduler is on: the reaper that stops an idle function, the sync that ends a start that serves
//     nothing and the minute charge are scheduler jobs;
//   - a sealing key is configured: without one no secret and no registry password can be stored;
//   - a sandbox-proxy replica of this provider reported to this portal within FunctionGate.ProxyWindow. A report is taken
//     only with the provider's function wake token, so this is the evidence that the wake path works at this site:
//     the proxy runs the function mode, holds the token, reaches the portal, and the portal answers it;
//   - billing is not the external provider;
//   - (warning) an enabled price plan has a rule for resource type `function`.
func (h *Handler) functionGates(ctx context.Context, es *externalservice.ExternalService) []sandbox.Gate {
	g := h.functionGate
	rows := []sandbox.Gate{
		{Name: gateFunctionScheduler, OK: h.jobsScheduler, Detail: "the job scheduler is on: the reaper stops idle functions every 15 seconds"},
		{Name: gateFunctionKey, OK: g.SealingKey, Detail: "a sealing key is configured: function secrets are stored sealed"},
		{Name: gateFunctionProxy},
		{Name: gateFunctionBilling, OK: !g.ExternalBilling, Detail: "usage is rated by this ProxCloud, not by an external billing provider"},
		{Name: gateFunctionPrice, OK: true, Detail: "an enabled price plan has a rule for resource type function"},
	}
	if !h.jobsScheduler {
		rows[0].Detail = "the job scheduler is off: nothing would stop an idle function or charge a running one. Turn it on (./proxcloud-up.sh install --scheduler, or PROXCLOUD_JOBS_SCHEDULER_ENABLED=true)"
	}
	if !g.SealingKey {
		rows[1].Detail = "the portal has no encryption key: function secrets and registry passwords could not be stored. Set an encryption key of 32 characters or more"
	}
	seconds := int(g.ProxyWindow / time.Second)
	if g.Proxies == nil {
		rows[2].Detail = "this API cannot read the sandbox-proxy reports"
	} else if n, err := g.Proxies(ctx, es.ID); err != nil {
		rows[2].Detail = "the sandbox-proxy reports could not be read"
	} else if n == 0 {
		rows[2].Detail = fmt.Sprintf("no sandbox-proxy replica of this provider has reported in the last %d seconds: apply the installation with Functions ticked and wait for the proxy to roll", seconds)
	} else {
		rows[2].OK, rows[2].Detail = true, fmt.Sprintf("%d sandbox-proxy replica(s) reported in the last %d seconds", n, seconds)
	}
	if g.ExternalBilling {
		rows[3].Detail = "billing is the external provider (PROXCLOUD_BILLING_PROVIDER=external): it is not sent the running intervals of a function and would bill its idle time. Functions are not offered with it"
	}
	if priced, err := h.functionPriced(ctx); err != nil {
		rows[4].OK, rows[4].Detail = false, "the price rules could not be read"
	} else if !priced {
		rows[4].OK = false
		rows[4].Detail = "no enabled price plan has a rule for resource type function: functions run and bill nothing. A rule added later bills a running function back to the start of the billing cycle or to its start, whichever is later."
	}
	return rows
}

// functionPriced reports whether an enabled price plan holds a rule for resource type `function`.
func (h *Handler) functionPriced(ctx context.Context) (bool, error) {
	if h.repo == nil {
		return false, nil
	}
	plans, err := h.repo.c(pricePlanRuleCollection).Distinct(ctx, "pricePlanId", pgdoc.M{"resourceType": "function"})
	if err != nil || len(plans) == 0 {
		return false, err
	}
	return h.repo.c(pricePlanCollection).Exists(ctx, pgdoc.M{"_id": pgdoc.M{"$in": plans}, "enabled": true})
}

// checkFunctionsEnabled is the gate of a sandbox provider doc whose functions block is switched on, whatever the
// provider's status. Off, nothing is asked: the block (limits, the default quota) is stored as written, and off is
// always accepted (the reaper then stops every function of the provider within its period).
//
// On, the scheduler, the sealing key and the billing provider must hold at every save. The proxy's report is asked
// for at the save that switches the block on (the stored document does not have it on, or there is none): it is the
// proof that the wake path works here, taken once. A later save of a provider whose functions run is not refused
// because its proxies were restarting at that moment: that refusal could only be answered by switching functions
// off, which stops them all.
func (h *Handler) checkFunctionsEnabled(ctx context.Context, es *externalservice.ExternalService) *httpx.HTTPError {
	if !es.SandboxConfig().Functions.Enabled {
		return nil
	}
	switchingOn := true
	if h.repo != nil && es.ID != "" {
		stored, err := h.repo.FindDoc(ctx, externalServiceCollection, es.ID)
		if err != nil {
			return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "The provider could not be read: the functions block was not checked and nothing was stored.")
		}
		if was := h.sandboxOf(stored); stored != nil && was != nil {
			switchingOn = !was.SandboxConfig().Functions.Enabled
		}
	}
	var failed []string
	for _, g := range h.functionGates(ctx, es) {
		if g.OK || g.Name == gateFunctionPrice || (g.Name == gateFunctionProxy && !switchingOn) {
			continue
		}
		failed = append(failed, g.Name+": "+g.Detail)
	}
	if len(failed) == 0 {
		return nil
	}
	return httpx.NewError(http.StatusConflict, http.StatusConflict,
		"Functions cannot be on for this sandbox provider: "+strings.Join(failed, "; ")+". Store the provider with config.sandbox.functions.enabled false until this holds (POST /service/{id}/sandbox/release-check shows each row).")
}
