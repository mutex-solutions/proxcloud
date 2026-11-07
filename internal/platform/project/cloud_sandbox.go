package project

// cloud_sandbox.go — the SANDBOX legs of cloudCreate / cloudDelete / cloudAction /
// cloudResourceList for a sandbox provider (gVisor pods on the sandbox cluster; nothing on PVE,
// dbaas precedent). See internal/cloud/sandbox and docs/managed-sandbox.md §3.
//
// What adds usage (create, exec, a longer timeout) passes three checks first: the suspension gate
// (refuseNewUsage, then the billing profile), the provider's release gates (sandboxReleaseGate) and,
// on create, the project's sandbox quota (D10, checked against the live cluster).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// sandboxService builds the service for es, writing the 503 itself when it cannot.
func (h *Handler) sandboxService(w http.ResponseWriter, es *externalservice.ExternalService) (*sandbox.Service, bool) {
	if h.sandboxFor == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "sandboxes are not configured")
		return nil, false
	}
	ss, err := h.sandboxFor(es)
	if err != nil {
		slog.Error("sandbox: build service", "service", es.ID, "err", err)
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "sandboxes are not available")
		return nil, false
	}
	return ss, true
}

// sandboxBillingGate refuses new sandbox usage (create, exec, a longer timeout) for a project that
// cannot be billed: refuseNewUsage first (deleting or DISABLED, an operator's disable and a billing
// suspension alike), then the billing profile (own, or the org's) must not be SUSPENDED, which
// covers the moment between a profile's suspension and the walk that disables its projects.
// Sandboxes start in seconds and an agent can start many, so this sits on the request itself.
func (h *Handler) sandboxBillingGate(ctx context.Context, proj *Project) error {
	if err := refuseNewUsage(proj); err != nil {
		return err
	}
	if proj.Status != StatusEnabled {
		return errSandboxForbidden("the project is not active")
	}
	if h.billing == nil {
		return nil
	}
	bpID := h.resolveBillingProfileID(ctx, proj)
	if bpID == "" {
		return nil // billing not set up for this project: nothing to gate on (same as other resources)
	}
	profile, err := h.billing.FindByID(ctx, bpID)
	if err != nil {
		return err
	}
	if profile != nil && profile.Status == billing.StatusSuspended {
		return errSandboxForbidden("the billing profile is suspended")
	}
	return nil
}

// sandboxGateTTL is how long a release-gate verdict is reused before the next create asks again.
const sandboxGateTTL = time.Minute

type gateVerdict struct {
	at  time.Time
	err error
}

// sandboxReleaseGate is nil when the provider's three release gates hold (docs/managed-sandbox.md §3). Fail closed: no
// gate checker wired, or any gate not holding, refuses. The verdict is cached per provider for sandboxGateTTL, a
// failure included, so a broken gate costs one check a minute and not one per request.
func (h *Handler) sandboxReleaseGate(ctx context.Context, es *externalservice.ExternalService) error {
	if h.sandboxGates == nil {
		return errors.New("no release-gate checker is wired")
	}
	h.gateMu.Lock()
	v, ok := h.gateCache[es.ID]
	h.gateMu.Unlock()
	if ok && time.Since(v.at) < sandboxGateTTL {
		return v.err
	}
	err := sandbox.GatesErr(h.sandboxGates(ctx, es))
	h.gateMu.Lock()
	if h.gateCache == nil {
		h.gateCache = map[string]gateVerdict{}
	}
	h.gateCache[es.ID] = gateVerdict{at: time.Now(), err: err}
	h.gateMu.Unlock()
	return err
}

// errSandboxesUnavailable is what a tenant sees while a release gate does not hold: the reason is the operator's.
var errSandboxesUnavailable = httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
	"Sandboxes are not available on this location right now. Try again later or contact your platform operator.")

// sandboxUsageGate is the suspension gate plus the release gates, for anything that adds usage.
func (h *Handler) sandboxUsageGate(ctx context.Context, proj *Project, es *externalservice.ExternalService) error {
	if err := h.sandboxBillingGate(ctx, proj); err != nil {
		return err
	}
	if err := h.sandboxReleaseGate(ctx, es); err != nil {
		slog.Error("sandbox: release gates do not hold, usage refused", "service", es.ID, "project", proj.ID, "err", err)
		return errSandboxesUnavailable
	}
	return nil
}

type sandboxForbidden string

func (e sandboxForbidden) Error() string { return string(e) }

func errSandboxForbidden(msg string) error { return sandboxForbidden(msg) }

// sandboxFail writes a sandbox error as HTTP (see sandboxHTTPError); anything unmapped goes
// through h.fail.
func (h *Handler) sandboxFail(w http.ResponseWriter, err error) {
	if status, msg, ok := sandboxHTTPError(err); ok {
		if status == http.StatusServiceUnavailable {
			slog.Error("sandbox: cluster error", "err", err)
		}
		httpx.Err(w, status, status, msg)
		return
	}
	h.fail(w, err)
}

// sandboxHTTPError is the sandbox error contract (docs/managed-sandbox.md). Errors coming back
// from the sandbox cluster's API server are classified here rather than surfacing as a 500:
//   - ResourceQuota rejections ("exceeded quota", the backstop behind the create-time limits) → 429
//   - Kubernetes validation (400/422) and exec start failures (e.g. executable not found) → 400
//   - an unreachable cluster or any other Kubernetes error → 503 (operator problem, logged)
func sandboxHTTPError(err error) (status int, msg string, ok bool) {
	var forbidden sandboxForbidden
	var kerr *kamajik8s.APIError
	switch {
	case errors.As(err, &forbidden):
		return http.StatusForbidden, "Cannot start a sandbox: " + err.Error(), true
	case errors.Is(err, sandbox.ErrQuotaExceeded):
		return http.StatusTooManyRequests, err.Error(), true
	case errors.Is(err, sandbox.ErrNoGPUCapacity):
		return http.StatusConflict, err.Error(), true
	case errors.Is(err, sandbox.ErrNotFound):
		return http.StatusNotFound, "sandbox not found", true
	case errors.Is(err, kamajik8s.ErrUnreachable):
		return http.StatusServiceUnavailable, "sandboxes are not available", true
	case errors.As(err, &kerr):
		switch {
		case kerr.Status == http.StatusForbidden && strings.Contains(kerr.Message, "exceeded quota"):
			return http.StatusTooManyRequests, sandbox.ErrQuotaExceeded.Error() + ": the project's sandbox resources are in use", true
		case kerr.Status == http.StatusBadRequest || kerr.Status == http.StatusUnprocessableEntity:
			return http.StatusBadRequest, kerr.Message, true
		default:
			return http.StatusServiceUnavailable, "sandboxes are not available", true
		}
	}
	return 0, "", false
}

// sandboxCreate handles the SANDBOX leg of cloudCreate.
func (h *Handler) sandboxCreate(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, req providers.CreateRequest) {
	if req.Type != cloud.TypeSandbox {
		h.fail(w, httpx.BadRequest("A sandbox service only provisions SANDBOX resources"))
		return
	}
	if err := h.sandboxUsageGate(r.Context(), proj, es); err != nil {
		h.sandboxFail(w, err)
		return
	}
	ss, ok := h.sandboxService(w, es)
	if !ok {
		return
	}
	spec, err := sandbox.SpecFromData(proj.ID, req.Data)
	if err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	q := es.SandboxProjectQuota(proj.Quota)
	spec.Quota = &q
	if err := spec.Validate(ss.Config()); err != nil {
		h.fail(w, httpx.BadRequest(err.Error()))
		return
	}
	saved, err := h.createSandboxResource(r.Context(), proj, es, ss, &spec)
	if err != nil {
		h.sandboxFail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

// createSandboxResource creates the sandbox on the cluster and its cache row (shared by the
// native API and the E2B-compatible one). spec must already be validated.
func (h *Handler) createSandboxResource(ctx context.Context, proj *Project, es *externalservice.ExternalService, ss *sandbox.Service, spec *sandbox.Spec) (*cloud.CloudResource, error) {
	data, err := ss.CreateSandbox(ctx, spec)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	region := proj.ServiceRegion(es.ID)
	if region == "" {
		region = es.SandboxRegion()
	}
	saved, err := h.cloud.Insert(ctx, &cloud.CloudResource{
		ServiceID: es.ID, Region: region, ProjectID: proj.ID,
		Type: cloud.TypeSandbox, ExternalID: spec.ID,
		Data: data, CreatedAt: &now, UpdatedAt: &now,
	})
	if err != nil {
		// The CR exists but the row does not: delete the CR rather than leave an unbilled sandbox.
		if derr := ss.DeleteSandbox(context.WithoutCancel(ctx), proj.ID, spec.ID); derr != nil {
			slog.Error("sandbox: delete after failed cache insert", "sandbox", spec.ID, "err", derr)
		}
		return nil, err
	}
	return saved, nil
}

// sandboxDelete handles the SANDBOX leg of cloudDelete: delete the CR (the pod goes with it) and
// end the cache row. A sandbox that ran keeps its row as DELETED for a few minutes so the billing
// job charges it up to now (sandbox.SyncProvider.Linger archives it afterwards); one that never
// ran is archived at once.
func (h *Handler) sandboxDelete(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) error {
	if h.sandboxFor == nil {
		return fmt.Errorf("sandboxes are not configured")
	}
	if cr.Data["status"] == sandbox.StatusDeleted {
		return nil // already ended; its end time must not move
	}
	ss, err := h.sandboxFor(es)
	if err != nil {
		return err
	}
	// Billing reads the archived row: refresh it first so a sandbox deleted before any list or
	// sync still carries RUNNING + runningSince instead of the PENDING payload from create.
	if live, err := ss.GetSandbox(ctx, proj.ID, cr.ExternalID); err == nil {
		cr.Data = live
	}
	if err := ss.DeleteSandbox(ctx, proj.ID, cr.ExternalID); err != nil {
		return err
	}
	now := time.Now().UTC()
	if rs, _ := cr.Data["runningSince"].(string); rs != "" {
		cr.Data = sandbox.MarkDeleted(cr.Data, now)
		cr.UpdatedAt = &now
		_, err := h.cloud.Update(ctx, cr)
		return err
	}
	return h.cloud.DeleteAndArchive(ctx, cr, now)
}

// sandboxAction handles SANDBOX actions. Returns false when the action is not a sandbox action
// (the caller falls through to its generic handling).
//
//	SET_TIMEOUT {timeoutSeconds}: shutdown moves to now+timeoutSeconds (keepalive or shorten)
//	REFRESH {}:                   the live state of this sandbox
//	EXEC {command:[...] | cmd:"…", stdin?, timeoutSeconds?}: run one command, wait for it,
//	                              → {result: {stdout, stderr, exitCode, truncated}}
//	GET_LOGS {tailLines?}:        the newest lines of its output → {result: {logs}}
//
// Like every other cloud action, the payload is wrapped in {result: …}. EXEC and SET_TIMEOUT add usage, so they pass
// the suspension and release gates; cloudAction's allow-list has already refused them for a DISABLED project, while
// GET_LOGS (a read) and the delete stay open.
func (h *Handler) sandboxAction(w http.ResponseWriter, r *http.Request, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if !es.IsSandbox() {
		return false
	}
	switch action {
	case "SET_TIMEOUT", "EXEC":
		if err := h.sandboxUsageGate(r.Context(), proj, es); err != nil {
			h.sandboxFail(w, err)
			return true
		}
	}
	switch action {
	case "SET_TIMEOUT", "REFRESH":
	case "EXEC":
		h.sandboxExec(w, r, proj, es, cr, data)
		return true
	case "GET_LOGS":
		h.sandboxLogs(w, r, proj, es, cr, data)
		return true
	default:
		h.fail(w, httpx.BadRequest("unknown sandbox action "+action))
		return true
	}
	ss, ok := h.sandboxService(w, es)
	if !ok {
		return true
	}
	var (
		live map[string]any
		err  error
	)
	if action == "SET_TIMEOUT" {
		live, err = ss.SetTimeout(r.Context(), proj.ID, cr.ExternalID, intAny(data["timeoutSeconds"]))
	} else {
		live, err = ss.GetSandbox(r.Context(), proj.ID, cr.ExternalID)
	}
	if errors.Is(err, sandbox.ErrBadTimeout) {
		h.fail(w, httpx.BadRequest(err.Error()))
		return true
	}
	if err != nil {
		h.sandboxFail(w, err)
		return true
	}
	cr.Data = live
	now := time.Now().UTC()
	cr.UpdatedAt = &now
	if _, err := h.cloud.Update(r.Context(), cr); err != nil {
		slog.Warn("sandbox: cache update after action", "sandbox", cr.ExternalID, "err", err)
	}
	httpx.OK(w, map[string]any{"result": *cr})
	return true
}

// sandboxExec runs one command in the sandbox (EXEC action). `cmd` is a shell string run with
// sh -c; `command` is an argv array run as-is. The output is returned, never stored.
func (h *Handler) sandboxExec(w http.ResponseWriter, r *http.Request, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, data map[string]any) {
	ss, ok := h.sandboxService(w, es)
	if !ok {
		return
	}
	var argv []string
	if raw, ok := data["command"].([]any); ok {
		for _, a := range raw {
			s, ok := a.(string)
			if !ok {
				h.fail(w, httpx.BadRequest("command must be an array of strings"))
				return
			}
			argv = append(argv, s)
		}
	}
	if c, _ := data["cmd"].(string); c != "" {
		if len(argv) > 0 {
			h.fail(w, httpx.BadRequest("pass either cmd or command, not both"))
			return
		}
		argv = []string{"sh", "-c", c}
	}
	var stdin []byte
	if in, ok := data["stdin"].(string); ok {
		stdin = []byte(in)
	}
	timeout := time.Duration(intAny(data["timeoutSeconds"])) * time.Second
	res, err := ss.Exec(r.Context(), proj.ID, cr.ExternalID, argv, stdin, timeout)
	switch {
	case errors.Is(err, sandbox.ErrBadExec):
		h.fail(w, httpx.BadRequest(err.Error()))
	case errors.Is(err, sandbox.ErrNotRunning):
		httpx.Err(w, http.StatusConflict, http.StatusConflict, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		httpx.Err(w, http.StatusGatewayTimeout, http.StatusGatewayTimeout, "command did not finish within its timeout")
	case err != nil:
		h.sandboxFail(w, err)
	default:
		httpx.OK(w, map[string]any{"result": *res})
	}
}

// maxLogLines caps GET_LOGS.
const maxLogLines = 2000

// sandboxLogs answers GET_LOGS: the newest tailLines (default 200) of the sandbox's output.
func (h *Handler) sandboxLogs(w http.ResponseWriter, r *http.Request, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, data map[string]any) {
	ss, ok := h.sandboxService(w, es)
	if !ok {
		return
	}
	n := intAny(data["tailLines"])
	if n <= 0 {
		n = 200
	}
	logs, err := ss.Logs(r.Context(), proj.ID, cr.ExternalID, min(n, maxLogLines))
	if err != nil {
		h.sandboxFail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"result": map[string]any{"logs": logs}})
}

// DeleteProjectSandboxes deletes every live sandbox of the project on each of its sandbox providers (D9: a billing
// suspension ends sandboxes; nothing else is kept, since a sandbox is disposable). Each one's row is ended at the
// delete so billing stops there. A sandbox the cluster lists with no row yet is deleted too. Returns how many went;
// the first error is returned after trying the rest, so the walk's next pass retries what failed.
func (h *Handler) DeleteProjectSandboxes(ctx context.Context, p *Project) (int, error) {
	if h.sandboxFor == nil {
		return 0, nil
	}
	n := 0
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if err != nil || es == nil || !es.IsSandbox() {
			continue
		}
		ss, err := h.sandboxFor(es)
		if err != nil {
			keep(err)
			continue
		}
		live, err := ss.ListSandboxes(ctx, p.ID)
		if err != nil {
			keep(err)
			continue
		}
		rows, err := h.cloud.FindByServiceProjectAndType(ctx, svcID, p.ID, cloud.TypeSandbox)
		if err != nil {
			keep(err)
			continue
		}
		byID := map[string]*cloud.CloudResource{}
		for i := range rows {
			byID[rows[i].ExternalID] = &rows[i]
		}
		for id := range live {
			if cr := byID[id]; cr != nil {
				err = h.sandboxDelete(ctx, es, p, cr)
			} else {
				err = ss.DeleteSandbox(ctx, p.ID, id)
			}
			if err != nil {
				keep(fmt.Errorf("sandbox %s: %w", id, err))
				continue
			}
			n++
		}
	}
	return n, first
}

// teardownSandboxes is the project-deletion leg for sandboxes: every SANDBOX row's sandbox is deleted and its row ended
// and archived, then each sandbox provider's project namespace goes (it cascades anything the rows missed). It returns
// the rows of other kinds, and whether anything is left to retry.
func (h *Handler) teardownSandboxes(ctx context.Context, p *Project, rows []cloud.CloudResource) ([]cloud.CloudResource, bool) {
	var left []cloud.CloudResource
	failed := false
	now := time.Now().UTC()
	for i := range rows {
		res := &rows[i]
		if res.Type != cloud.TypeSandbox {
			left = append(left, *res)
			continue
		}
		es, err := h.esSvc.Get(ctx, res.ServiceID)
		if err != nil || es == nil || !es.IsSandbox() || h.sandboxFor == nil {
			left = append(left, *res)
			continue
		}
		ss, err := h.sandboxFor(es)
		if err == nil {
			err = ss.DeleteSandbox(ctx, p.ID, res.ExternalID)
		}
		if err != nil {
			slog.Error("teardown: delete sandbox", "project", p.ID, "sandbox", res.ExternalID, "err", err)
			left, failed = append(left, *res), true
			continue
		}
		if res.Data["status"] != sandbox.StatusDeleted {
			res.Data = sandbox.MarkDeleted(res.Data, now)
		}
		if err := h.cloud.DeleteAndArchive(ctx, res, now); err != nil {
			slog.Error("teardown: archive sandbox row", "project", p.ID, "sandbox", res.ExternalID, "err", err)
			left, failed = append(left, *res), true
		}
	}
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if err != nil || es == nil || !es.IsSandbox() {
			continue
		}
		if h.sandboxFor == nil {
			failed = true
			continue
		}
		ss, err := h.sandboxFor(es)
		if err == nil {
			err = ss.DeleteProjectNamespace(ctx, p.ID)
		}
		if err != nil {
			slog.Error("teardown: delete sandbox namespace", "project", p.ID, "serviceId", svcID, "err", err)
			failed = true
		}
	}
	return left, failed
}

// sandboxServiceOf returns the id of the project's first sandbox service ("" if none). A SANDBOX
// create that names no service goes there instead of the project's default (Proxmox) one, so
// API and MCP callers do not have to look the id up.
func (h *Handler) sandboxServiceOf(ctx context.Context, p *Project) string {
	for _, id := range p.ServiceIDs() {
		if es, err := h.esSvc.Get(ctx, id); err == nil && es != nil && es.IsSandbox() && !es.IsDisabled() {
			return id
		}
	}
	return ""
}

// refreshSandboxes live-reconciles the project's SANDBOX cache off the sandbox cluster: the
// client list's read-through (a sandbox becomes RUNNING seconds after create; the 15-minute sync
// is far too stale). Best-effort.
func (h *Handler) refreshSandboxes(ctx context.Context, p *Project) {
	if h.sandboxFor == nil {
		return
	}
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if err != nil || es == nil || !es.IsSandbox() {
			continue
		}
		ss, err := h.sandboxFor(es)
		if err != nil {
			continue
		}
		if _, err := providers.Reconcile(ctx, ss.SyncProvider(es.SandboxRegion(), p.ID), h.cloud, es.ID, time.Now().UTC()); err != nil {
			slog.Warn("sandbox: live list refresh", "project", p.ID, "service", svcID, "err", err)
		}
	}
}
