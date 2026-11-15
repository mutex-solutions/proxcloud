package admin

// platform.go — the managed-services platform (docs/adr/0009-managed-services-platform-lifecycle.md, design §3.5):
//
//	GET  /admin/platform/bom                              the BOM this release embeds, with its sha256      (service:read)
//	GET  /admin/platform/installations                    every installation                                (service:read)
//	POST /admin/platform/installations                    a new one, in phase draft                         (service:manage)
//	GET  /admin/platform/installations/{id}               one                                               (service:read)
//	PUT  /admin/platform/installations/{id}               replace name and spec; refused during a run, on a
//	                                                      stale rev and for fixed fields                    (service:manage)
//	PUT  /admin/platform/installations/{id}/credentials   the CAPI and CSI token secrets, write-only        (service:manage)
//	POST /admin/platform/installations/{id}/preflight     read-only checks against Proxmox                  (service:manage)
//	GET  /admin/platform/installations/{id}/plan          the installed BOM against this release's          (service:read)
//	POST /admin/platform/installations/{id}/runs          {kind: apply|remove} → 202 {runId}                (service:manage)
//	GET  /admin/platform/installations/{id}/runs[/{runId}] the newest runs, or one, with their stages       (service:read)
//	POST /admin/platform/installations/{id}/runs/{runId}/cancel                                             (service:manage)
//
// The installation routes answer 404 platform_disabled unless PROXCLOUD_PLATFORM_ENABLED is on (SetPlatform); a run
// also needs the runner (SetPlatformRuns, with PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED), else 409 platform_disabled. No
// route returns a credential: an installation shows only which token ids are stored, a run only fixed text and codes.
// The preflight changes nothing, so the audit middleware skips it; it needs service:manage because it uses the stored
// platform tokens. The seed kubeconfig and the seed credential rotation come with stages 6 to 11.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// SetPlatform wires the platform store (PROXCLOUD_PLATFORM_ENABLED). Unwired, the installation routes answer 404.
func (h *Handler) SetPlatform(s *platformlife.Store) { h.platform = s }

// SetPlatformRuns lets POST …/runs enqueue on jobs; cmd/api calls it only where the platform runner runs.
func (h *Handler) SetPlatformRuns(jobs *durablejobs.Store) { h.platformJobs = jobs }

func (h *Handler) routePlatform(r chi.Router) {
	r.Get("/platform/bom", h.platformBOM)
	r.Get("/platform/installations", h.platformList)
	r.Post("/platform/installations", h.platformCreate)
	r.Get("/platform/installations/{id}", h.platformGet)
	r.Put("/platform/installations/{id}", h.platformUpdate)
	r.Put("/platform/installations/{id}/credentials", h.platformCredentials)
	r.Post("/platform/installations/{id}/preflight", h.platformPreflight)
	r.Get("/platform/installations/{id}/plan", h.platformPlan)
	r.Post("/platform/installations/{id}/runs", h.platformRunStart)
	r.Get("/platform/installations/{id}/runs", h.platformRuns)
	r.Get("/platform/installations/{id}/runs/{runId}", h.platformRun)
	r.Post("/platform/installations/{id}/runs/{runId}/cancel", h.platformRunCancel)
}

func (h *Handler) platformBOM(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, serviceReadPerm) {
		return
	}
	bom, err := platformlife.Current()
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, bom)
}

// platformGate checks the permission, then the flag; false = the answer went out.
func (h *Handler) platformGate(w http.ResponseWriter, r *http.Request, perm string) bool {
	if !h.require(w, r, perm) {
		return false
	}
	if h.platform == nil {
		platformErr(w, http.StatusNotFound, platformlife.CodePlatformDisabled,
			"Managed services are off on this site: set PROXCLOUD_PLATFORM_ENABLED=true (docs/adr/0009-managed-services-platform-lifecycle.md)", nil)
		return false
	}
	return true
}

func platformErr(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	d := map[string]any{"code": code}
	for k, v := range details {
		d[k] = v
	}
	httpx.WriteError(w, httpx.NewError(status, status, msg).WithDetails(d))
}

// platformStoreErr answers an error of the store or of a change; false when err is nil.
func platformStoreErr(w http.ResponseWriter, err error) bool {
	var inv *invalidInstallation
	switch {
	case err == nil:
		return false
	case errors.As(err, &inv):
		platformErr(w, inv.status, inv.code, inv.msg, map[string]any{"problems": inv.problems})
	case errors.Is(err, platformlife.ErrNotFound):
		platformErr(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, platformlife.ErrDuplicate):
		platformErr(w, http.StatusConflict, "installation_exists", err.Error(), nil)
	case errors.Is(err, platformlife.ErrRevConflict):
		platformErr(w, http.StatusConflict, "rev_conflict", err.Error(), nil)
	case errors.Is(err, platformlife.ErrRunActive):
		platformErr(w, http.StatusConflict, "run_active", err.Error(), nil)
	case errors.Is(err, platformlife.ErrNoEncryptionKey):
		platformErr(w, http.StatusConflict, "no_encryption_key", err.Error(), nil)
	case errors.Is(err, platformlife.ErrBadKind):
		platformErr(w, http.StatusBadRequest, "invalid_kind", err.Error(), nil)
	case errors.Is(err, platformlife.ErrRunNotFound):
		platformErr(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, platformlife.ErrRunFinished):
		platformErr(w, http.StatusConflict, "run_finished", err.Error(), nil)
	case errors.Is(err, platformlife.ErrBOMNotHere):
		platformErr(w, http.StatusConflict, platformlife.CodeBOMUnsupported, err.Error(), nil)
	default:
		httpx.WriteError(w, err)
	}
	return true
}

// invalidInstallation is a refused installation: 400 invalid_installation (its own rules or the site's) or 409
// fixed_fields, with each problem.
type invalidInstallation struct {
	status    int
	code, msg string
	problems  []string
}

func (e *invalidInstallation) Error() string { return e.msg }

// installationView is an installation as the routes answer it: no credential, only which token ids are stored.
type installationView struct {
	*platformlife.Installation
	Credentials struct {
		CAPI *tokenView `json:"capi,omitempty"`
		CSI  *tokenView `json:"csi,omitempty"`
	} `json:"credentials"`
}

type tokenView struct {
	TokenID string `json:"tokenId"`
}

func (h *Handler) platformView(r *http.Request, in *platformlife.Installation) (*installationView, error) {
	v := &installationView{Installation: in}
	sec, err := h.platform.Secrets(r.Context(), in.ID)
	if err != nil || sec == nil {
		return v, err
	}
	if sec.CAPI != nil {
		v.Credentials.CAPI = &tokenView{TokenID: sec.CAPI.TokenID}
	}
	if sec.CSI != nil {
		v.Credentials.CSI = &tokenView{TokenID: sec.CSI.TokenID}
	}
	return v, nil
}

func (h *Handler) platformList(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, serviceReadPerm) {
		return
	}
	all, err := h.platform.List(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]*installationView, 0, len(all))
	for i := range all {
		v, err := h.platformView(r, &all[i])
		if httpx.WriteError(w, err) {
			return
		}
		out = append(out, v)
	}
	httpx.List(w, out)
}

func (h *Handler) platformGet(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, serviceReadPerm) {
		return
	}
	in, err := h.platform.Get(r.Context(), chi.URLParam(r, "id"))
	if platformStoreErr(w, err) {
		return
	}
	v, err := h.platformView(r, in)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, v)
}

// installationRequest is the body of a create and of an update. serviceId, provisionedBy and layout may be left out of
// an update; rev is the one the update read.
type installationRequest struct {
	Name          string            `json:"name"`
	ServiceID     string            `json:"serviceId"`
	ProvisionedBy string            `json:"provisionedBy"`
	Layout        string            `json:"layout"`
	Spec          platformlife.Spec `json:"spec"`
	Rev           int64             `json:"rev"`
}

// checked normalizes in and checks it on its own and against its provider's networks.
func (h *Handler) checked(r *http.Request, in *platformlife.Installation) error {
	es, err := h.esSvc.Get(r.Context(), in.ServiceID)
	if err != nil || es == nil || !es.IsProxmox() {
		return &invalidInstallation{status: http.StatusBadRequest, code: "invalid_installation", msg: "serviceId must name a Proxmox provider",
			problems: []string{"serviceId " + in.ServiceID + " is not a Proxmox provider"}}
	}
	if in.Spec.Storage.Import == "" {
		in.Spec.Storage.Import = es.PVEImportStorage()
	}
	in.Normalize()
	var problems []string
	if err := in.Validate(); err != nil {
		problems = strings.Split(err.Error(), "\n")
	}
	problems = append(problems, in.SiteProblems(platformlife.SiteOf(es))...)
	if len(problems) > 0 {
		return &invalidInstallation{status: http.StatusBadRequest, code: "invalid_installation", msg: "The installation is not valid", problems: problems}
	}
	return nil
}

func (h *Handler) platformCreate(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	var req installationRequest
	if !decodeStrictLimited(w, r, &req, 64<<10) {
		return
	}
	if h.esSvc == nil {
		httpx.WriteError(w, serviceNotFoundErr(req.ServiceID))
		return
	}
	in := &platformlife.Installation{Name: req.Name, ServiceID: req.ServiceID, ProvisionedBy: req.ProvisionedBy, Layout: req.Layout,
		Spec: req.Spec, CreatedBy: adminActor(r)}
	if platformStoreErr(w, h.checked(r, in)) || platformStoreErr(w, h.platform.Create(r.Context(), in)) {
		return
	}
	httpx.OK(w, &installationView{Installation: in})
}

func (h *Handler) platformUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	var req installationRequest
	if !decodeStrictLimited(w, r, &req, 64<<10) {
		return
	}
	before, after, err := h.platform.Update(r.Context(), chi.URLParam(r, "id"), req.Rev, func(old platformlife.Installation) (*platformlife.Installation, error) {
		next := old
		next.Name, next.Spec = req.Name, req.Spec
		if req.ServiceID != "" {
			next.ServiceID = req.ServiceID
		}
		if req.ProvisionedBy != "" {
			next.ProvisionedBy = req.ProvisionedBy
		}
		if req.Layout != "" {
			next.Layout = req.Layout
		}
		if h.esSvc == nil {
			return nil, serviceNotFoundErr(next.ServiceID)
		}
		if err := h.checked(r, &next); err != nil {
			return nil, err
		}
		if fixed := platformlife.FixedChanges(&old, &next); len(fixed) > 0 {
			return nil, &invalidInstallation{status: http.StatusConflict, code: "fixed_fields",
				msg: "These fields cannot change once the platform was installed (serviceId never): " + strings.Join(fixed, ", "), problems: fixed}
		}
		return &next, nil
	})
	if platformStoreErr(w, err) {
		return
	}
	audit.RecordSnapshots(r.Context(), asSnapshot(before), asSnapshot(after))
	v, err := h.platformView(r, after)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, v)
}

// asSnapshot is a document as the audit diff reads it. Only installations come here, never their secrets.
func asSnapshot(in *platformlife.Installation) map[string]any {
	b, _ := json.Marshal(in)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// pveTokenSecret is the shape of a PVE API token secret.
var pveTokenSecret = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// platformCredentials stores the CAPI and the CSI token. Each is optional; the token ids are the ones the
// managed-services script creates, whose ACL rows and fences the preflight checks. The body is never echoed, logged or
// audited beyond the middleware's event (actor, installation id).
func (h *Handler) platformCredentials(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	var req struct {
		CAPI *platformlife.TokenCredential `json:"capi"`
		CSI  *platformlife.TokenCredential `json:"csi"`
	}
	if !decodeStrictLimited(w, r, &req, 4<<10) {
		return
	}
	var problems []string
	for _, c := range []struct {
		tok  *platformlife.TokenCredential
		want string
	}{{req.CAPI, pve.CAPITokenID}, {req.CSI, pve.CSITokenID}} {
		switch {
		case c.tok == nil:
		case c.tok.TokenID != c.want:
			problems = append(problems, "tokenId must be "+c.want)
		case !pveTokenSecret.MatchString(c.tok.Secret):
			problems = append(problems, "the secret of "+c.want+" is not a PVE token secret (a UUID)")
		}
	}
	if req.CAPI == nil && req.CSI == nil {
		problems = append(problems, "give capi, csi or both")
	}
	if len(problems) > 0 {
		platformErr(w, http.StatusBadRequest, "invalid_credentials", "The credentials are not valid", map[string]any{"problems": problems})
		return
	}
	id := chi.URLParam(r, "id")
	in, err := h.platform.Get(r.Context(), id)
	if platformStoreErr(w, err) || platformStoreErr(w, h.platform.SaveCredentials(r.Context(), id, req.CAPI, req.CSI)) {
		return
	}
	v, err := h.platformView(r, in)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, v)
}

func (h *Handler) platformPreflight(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	in, err := h.platform.Get(r.Context(), chi.URLParam(r, "id"))
	if platformStoreErr(w, err) {
		return
	}
	checks, err := h.PlatformPreflight(r.Context(), in)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, map[string]any{"ok": platformlife.OK(checks), "checks": checks})
}

// PlatformPreflight runs the read-only checks of an installation against its provider; a run's first stage calls it.
func (h *Handler) PlatformPreflight(ctx context.Context, in *platformlife.Installation) ([]platformlife.Check, error) {
	if h.esSvc == nil {
		return nil, serviceNotFoundErr(in.ServiceID)
	}
	es, err := h.esSvc.Get(ctx, in.ServiceID)
	if err != nil || es == nil {
		return nil, serviceNotFoundErr(in.ServiceID)
	}
	if h.pveFor == nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Proxmox access is not configured")
	}
	portal, err := h.pveFor(es)
	if err != nil {
		return nil, httpx.BadRequest("Proxmox connection failed: " + err.Error())
	}
	sec, err := h.platform.Secrets(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if sec == nil {
		sec = &platformlife.Secrets{}
	}
	pf := platformlife.Preflight{Installation: in, Site: platformlife.SiteOf(es), Allow: es.PVESandboxEndpoints(),
		ProviderImport: es.PVEImportStorage(), Portal: portal,
		TemplateUpdatesVM: h.templateUpdates != nil && h.templateUpdates.VMEnabled}
	if pf.CAPI, err = platformlife.TokenClient(es, sec.CAPI); err != nil {
		return nil, err
	}
	if pf.CSI, err = platformlife.TokenClient(es, sec.CSI); err != nil {
		return nil, err
	}
	if n, err := h.githubCount(ctx); err == nil {
		pf.GitHub = n > 0
	}
	if h.templateUpdates != nil {
		rec := h.templateUpdates.Receipts
		pf.Blocked = func(ctx context.Context, digest string) (bool, error) {
			b, err := rec.Blocked(ctx, digest)
			return b != nil, err
		}
	}
	return pf.Run(ctx), nil
}

func (h *Handler) platformPlan(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, serviceReadPerm) {
		return
	}
	in, err := h.platform.Get(r.Context(), chi.URLParam(r, "id"))
	if platformStoreErr(w, err) {
		return
	}
	plan, err := platformlife.PlanFor(in.Status.BOM)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, plan)
}

// platformRunStart starts an apply or a remove: 202 with the run's id, 409 run_active while another run is unfinished.
func (h *Handler) platformRunStart(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	if h.platformJobs == nil {
		platformErr(w, http.StatusConflict, platformlife.CodePlatformDisabled,
			"Runs need the platform runner: set PROXCLOUD_PLATFORM_ENABLED=true and PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true", nil)
		return
	}
	var req struct {
		Kind string `json:"kind"`
	}
	if !decodeStrictLimited(w, r, &req, 1<<10) {
		return
	}
	run, err := h.platform.StartRun(r.Context(), h.platformJobs, chi.URLParam(r, "id"), req.Kind, adminActor(r))
	if platformStoreErr(w, err) {
		return
	}
	httpx.Raw(w, http.StatusAccepted, httpx.Response[map[string]string]{Data: &map[string]string{"runId": run.ID}})
}

func (h *Handler) platformRuns(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, serviceReadPerm) {
		return
	}
	runs, err := h.platform.Runs(r.Context(), chi.URLParam(r, "id"), 20)
	if httpx.WriteError(w, err) {
		return
	}
	for i := range runs {
		if runs[i], err = platformlife.RunView(r.Context(), h.platformJobs, runs[i]); httpx.WriteError(w, err) {
			return
		}
	}
	httpx.List(w, runs)
}

func (h *Handler) platformRun(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, serviceReadPerm) {
		return
	}
	run, err := h.platform.Run(r.Context(), chi.URLParam(r, "runId"))
	if httpx.WriteError(w, err) {
		return
	}
	if run == nil || run.InstallationID != chi.URLParam(r, "id") {
		platformStoreErr(w, platformlife.ErrRunNotFound)
		return
	}
	v, err := platformlife.RunView(r.Context(), h.platformJobs, *run)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, v)
}

// platformRunCancel asks a run to stop at its next claim or write; the run ends cancelled and the installation failed.
func (h *Handler) platformRunCancel(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	id, runID := chi.URLParam(r, "id"), chi.URLParam(r, "runId")
	run, err := h.platform.CancelRun(r.Context(), h.platformJobs, id, runID)
	if platformStoreErr(w, err) {
		return
	}
	v, err := platformlife.RunView(r.Context(), h.platformJobs, *run)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, v)
}
