package admin

// platform.go — the managed-services platform (docs/adr/0009-managed-services-platform-lifecycle.md, design §3.5):
//
//	GET  /admin/platform/bom                              the BOM this release embeds, with its sha256      (service:read)
//	GET  /admin/platform/installations                    every installation                                (service:read)
//	POST /admin/platform/installations                    a new one, in phase draft; "preset" fills what the
//	                                                      spec leaves out                                   (service:manage)
//	GET  /admin/platform/installations/{id}               one                                               (service:read)
//	PUT  /admin/platform/installations/{id}               replace name and spec; refused during a run, on a
//	                                                      stale rev and for fixed fields                    (service:manage)
//	PUT  /admin/platform/installations/{id}/credentials   the CAPI and CSI token secrets, write-only        (service:manage)
//	POST /admin/platform/installations/{id}/preflight     read-only checks against Proxmox                  (service:manage)
//	GET  /admin/platform/installations/{id}/plan          the installed BOM against this release's          (service:read)
//	GET  /admin/platform/installations/{id}?live=1        one, with the components' live sync and health    (service:read)
//	POST /admin/platform/installations/{id}/runs          {kind: apply|remove, confirm} → 202 {runId}       (service:manage)
//	GET  /admin/platform/installations/{id}/runs[/{runId}] the newest runs, or one, with their stages       (service:read)
//	POST /admin/platform/installations/{id}/runs/{runId}/cancel                                             (service:manage)
//	GET  /admin/platform/installations/{id}/seed-kubeconfig a one-hour cluster-admin kubeconfig of the seed  (service:manage)
//	GET  /admin/platform/installations/{id}/platform-kubeconfig a one-hour kubeconfig of the platform cluster that
//	                                                      reads, and opens its ArgoCD                        (service:manage)
//	POST /admin/platform/installations/{id}/seed-credential/rotate  a new stored seed credential; refused
//	                                                      during a run                                       (service:manage)
//
// The installation routes answer 404 platform_disabled unless PROXCLOUD_PLATFORM_ENABLED is on (SetPlatform); a run
// also needs the runner (SetPlatformRuns, with PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED), else 409 platform_disabled. No
// route returns a credential: an installation shows only which token ids are stored, a run only fixed text and codes.
// The preflight changes nothing, so the audit middleware skips it; it needs service:manage because it uses the stored
// platform tokens. The exceptions to "no credential" are seed-kubeconfig and platform-kubeconfig, the break-glass
// downloads: they need service:manage although they are GETs, and each writes its own audit entry (actor, which of the
// two and the expiry, never the token).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

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

// SetPlatformSDNLock wires the site's SDN apply lock for the tenant path test's throwaway network.
func (h *Handler) SetPlatformSDNLock(l func(ctx context.Context) (release func(), acquired bool, err error)) {
	h.platformSDNLock = l
}

func (h *Handler) routePlatform(r chi.Router) {
	r.Get("/platform/bom", h.platformBOM)
	r.Get("/platform/installations", h.platformList)
	r.Post("/platform/installations", h.platformCreate)
	r.Get("/platform/installations/{id}", h.platformGet)
	r.Put("/platform/installations/{id}", h.platformUpdate)
	r.Put("/platform/installations/{id}/credentials", h.platformCredentials)
	r.Post("/platform/installations/{id}/preflight", h.platformPreflight)
	r.Get("/platform/installations/{id}/tenant-path-probe", h.platformTenantProbe)
	r.Post("/platform/installations/{id}/tenant-path-probe", h.platformTenantProbeStart)
	r.Get("/platform/installations/{id}/plan", h.platformPlan)
	r.Post("/platform/installations/{id}/runs", h.platformRunStart)
	r.Get("/platform/installations/{id}/runs", h.platformRuns)
	r.Get("/platform/installations/{id}/runs/{runId}", h.platformRun)
	r.Post("/platform/installations/{id}/runs/{runId}/cancel", h.platformRunCancel)
	r.Get("/platform/installations/{id}/seed-kubeconfig", h.platformSeedKubeconfig)
	r.Get("/platform/installations/{id}/platform-kubeconfig", h.platformClusterKubeconfig)
	r.Post("/platform/installations/{id}/seed-credential/rotate", h.platformSeedRotate)
}

func (h *Handler) platformBOM(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, serviceReadPerm) {
		return
	}
	if sha := r.URL.Query().Get("sha256"); sha != "" {
		bom, ok := platformlife.Lookup(sha)
		if !ok {
			platformErr(w, http.StatusNotFound, platformlife.CodeBOMUnsupported, "This release does not embed a BOM with that sha256.", nil)
			return
		}
		httpx.OK(w, bom)
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
	var refused *platformlife.UpgradeRefused
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
	case errors.Is(err, platformlife.ErrBadConfirm):
		platformErr(w, http.StatusBadRequest, "invalid_confirmation", err.Error(), nil)
	case errors.Is(err, platformlife.ErrNoSeed):
		platformErr(w, http.StatusConflict, "no_seed", err.Error(), nil)
	case errors.Is(err, platformlife.ErrNoCluster):
		platformErr(w, http.StatusConflict, "no_cluster", err.Error(), nil)
	case errors.Is(err, platformlife.ErrBadKind):
		platformErr(w, http.StatusBadRequest, "invalid_kind", err.Error(), nil)
	case errors.Is(err, platformlife.ErrRunNotFound):
		platformErr(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, platformlife.ErrRunFinished):
		platformErr(w, http.StatusConflict, "run_finished", err.Error(), nil)
	case errors.Is(err, platformlife.ErrBOMNotHere):
		platformErr(w, http.StatusConflict, platformlife.CodeBOMUnsupported, err.Error(), nil)
	case errors.Is(err, platformlife.ErrBadTarget):
		platformErr(w, http.StatusBadRequest, "invalid_target", err.Error(), nil)
	case errors.As(err, &refused):
		platformErr(w, http.StatusConflict, platformlife.CodeUpgradeRefused, err.Error(), map[string]any{"reasons": refused.Reasons})
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
	// Live is set with ?live=1 only: read from the seed at request time, never stored.
	Live *platformlife.LiveState `json:"live,omitempty"`
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
	if r.URL.Query().Get("live") == "1" {
		sec, err := h.platform.Secrets(r.Context(), in.ID)
		if httpx.WriteError(w, err) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		live := platformlife.Live(ctx, in, sec)
		v.Live = &live
	}
	httpx.OK(w, v)
}

// installationRequest is the body of a create and of an update. serviceId, provisionedBy and layout may be left out of
// an update; rev is the one the update read. preset (homelab, standard or custom) fills what the spec leaves out
// (platformlife.ApplyPreset) before the checks and is not stored: the answer carries the whole spec.
type installationRequest struct {
	Name          string            `json:"name"`
	Preset        string            `json:"preset"`
	ServiceID     string            `json:"serviceId"`
	ProvisionedBy string            `json:"provisionedBy"`
	Layout        string            `json:"layout"`
	Spec          platformlife.Spec `json:"spec"`
	Rev           int64             `json:"rev"`
}

// checked fills in from preset, normalizes it and checks it on its own and against its provider's networks.
func (h *Handler) checked(r *http.Request, in *platformlife.Installation, preset string) error {
	if err := platformlife.ApplyPreset(preset, &in.Spec); err != nil {
		return &invalidInstallation{status: http.StatusBadRequest, code: "invalid_installation", msg: "The installation is not valid", problems: []string{err.Error()}}
	}
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
	if platformStoreErr(w, h.checked(r, in, req.Preset)) || platformStoreErr(w, h.platform.Create(r.Context(), in)) {
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
		if err := h.checked(r, &next, req.Preset); err != nil {
			return nil, err
		}
		if why := platformlife.ControlPlaneShrink(&old, &next); why != "" {
			return nil, &invalidInstallation{status: http.StatusConflict, code: "fixed_fields", msg: why, problems: []string{why}}
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

// platformTenantProbe answers the installation's last tenant path test (404 no_probe when none was run).
func (h *Handler) platformTenantProbe(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, serviceReadPerm) {
		return
	}
	p, err := h.platform.Probe(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if p == nil {
		platformErr(w, http.StatusNotFound, "no_probe", "The tenant path of this installation was not tested yet.", nil)
		return
	}
	// A probe cut off by an API restart reads as an error, so the page stops waiting for it.
	httpx.OK(w, p.View(time.Now()))
}

// platformTenantProbeStart starts "Test the tenant path" (platformlife.TenantProber) and answers at once with the
// probe as running; GET reads how it ends. It writes to Proxmox (a throwaway network and VM), so the admin audit
// middleware records it like every installation write, and it is refused during a run of the installation. A refusal (409, with its code) comes before anything is created.
func (h *Handler) platformTenantProbeStart(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	var req struct {
		Node string `json:"node"`
	}
	if r.ContentLength != 0 && !decodeStrictLimited(w, r, &req, 1<<10) {
		return
	}
	ctx := r.Context()
	in, err := h.platform.Get(ctx, chi.URLParam(r, "id"))
	if platformStoreErr(w, err) {
		return
	}
	if run, err := h.platform.ActiveRun(ctx, in.ID); httpx.WriteError(w, err) {
		return
	} else if run != nil {
		platformStoreErr(w, platformlife.ErrRunActive)
		return
	}
	if h.esSvc == nil || h.pveFor == nil {
		httpx.WriteError(w, serviceNotFoundErr(in.ServiceID))
		return
	}
	es, err := h.esSvc.Get(ctx, in.ServiceID)
	if err != nil || es == nil {
		httpx.WriteError(w, serviceNotFoundErr(in.ServiceID))
		return
	}
	portal, err := h.pveFor(es)
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest("Proxmox connection failed: "+err.Error()))
		return
	}
	prober := &platformlife.TenantProber{Portal: portal, Net: es.PVENetworkConfig(), SDNLock: h.platformSDNLock, Load: h.platform.Probe, Save: h.platform.SaveProbe}
	p, err := prober.Start(ctx, in, req.Node, adminActor(r))
	var refused *platformlife.ProbeRefused
	if errors.As(err, &refused) {
		platformErr(w, http.StatusConflict, refused.Code, refused.Message, nil)
		return
	}
	if httpx.WriteError(w, err) {
		return
	}
	// The probe outlives the request: its cleanup must run whatever the client does.
	started := *p
	started.Targets = slices.Clone(p.Targets) // Run writes each target's result
	go prober.Run(context.WithoutCancel(ctx), p)
	httpx.Raw(w, http.StatusAccepted, httpx.Response[platformlife.TenantProbe]{Data: &started})
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
		Portal: portal, ProviderDisabled: es.IsDisabled(),
		TemplateUpdatesVM: h.templateUpdates != nil && h.templateUpdates.VMEnabled}
	pf.MintStores, pf.MintZones = platformlife.MintScopeOf(es)
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
	// ?target=<sha256>: the BOM to plan the move to (default: this build's own).
	plan, err := platformlife.PlanFor(in, r.URL.Query().Get("target"))
	if platformStoreErr(w, err) {
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
		Kind    string `json:"kind"`
		Confirm string `json:"confirm"` // "remove <name>": lets a remove go on with the seed gone or the workload silent; "upgrade <name> to <release>" with targetBom
		// TargetBOM makes an apply an upgrade: the sha256 of this build's BOM, which the platform is moved to (GET …/plan
		// says whether it is allowed and what to type). Without it an apply stays on the installed BOM.
		TargetBOM string `json:"targetBom"`
	}
	if !decodeStrictLimited(w, r, &req, 1<<10) {
		return
	}
	run, err := h.platform.StartRun(r.Context(), h.platformJobs, chi.URLParam(r, "id"), req.Kind, adminActor(r), req.Confirm, req.TargetBOM)
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

// ActionBreakGlass is the audit action of a kubeconfig download, the seed's or the platform cluster's.
const ActionBreakGlass = "BREAK_GLASS"

// ActionRotateCredential is the audit action of a seed credential rotation.
const ActionRotateCredential = "ROTATE_CREDENTIAL"

// breakGlassEvent is the audit entry of a kubeconfig download: who, which installation, which of the two kubeconfigs
// (operation seed-kubeconfig or platform-kubeconfig) and when the token expires. It is built without the token or the
// kubeconfig, so it cannot carry them.
func breakGlassEvent(r *http.Request, in *platformlife.Installation, operation string, expires time.Time) audit.AuditEvent {
	return seedEvent(r, in, ActionBreakGlass, map[string]any{"operation": operation, "expiresAt": expires.UTC().Format(time.RFC3339)})
}

// seedRotateEvent is the audit entry of a seed credential rotation: who, which installation, and the names of the
// token Secret that was stored before and of the one stored now. Like breakGlassEvent it is built without a token.
func seedRotateEvent(r *http.Request, in *platformlife.Installation, from, to string) audit.AuditEvent {
	return seedEvent(r, in, ActionRotateCredential, map[string]any{"operation": "seed-credential-rotate", "fromSecret": from, "toSecret": to})
}

// seedRotateUnfinishedEvent is the entry of a rotation that answered 502, and whether there is one to write: only when
// the stored credential changed all the same (the store committed and a later step failed), so the old token Secrets
// may still be on the seed. A rotation that stored nothing changed nothing and has no entry.
func seedRotateUnfinishedEvent(r *http.Request, before, after *platformlife.Installation) (audit.AuditEvent, bool) {
	if before.Status.Seed == nil || after.Status.Seed == nil || before.Status.Seed.TokenSecret == after.Status.Seed.TokenSecret {
		return audit.AuditEvent{}, false
	}
	ev := seedRotateEvent(r, after, before.Status.Seed.TokenSecret, after.Status.Seed.TokenSecret)
	ev.Outcome, ev.ErrorMessage = audit.OutcomeFailure, "The rotation did not finish: the new credential is stored, and older token Secrets may still be on the seed."
	return ev, true
}

func seedEvent(r *http.Request, in *platformlife.Installation, action string, meta map[string]any) audit.AuditEvent {
	actor := adminActor(r)
	ev := audit.AdminEvent(actor, actor)
	ev.EventContext = audit.ContextPlatform
	ev.Action, ev.ResourceType, ev.ResourceID, ev.ResourceDisplayName = action, "PLATFORM_INSTALLATION", in.ID, in.Name
	ev.ResourceMetadata = meta
	ev.Outcome = audit.OutcomeSuccess
	if ev.Actor != nil {
		ev.Actor.IPAddress = clientIP(r)
		ev.Actor.UserAgent = r.UserAgent()
	}
	return ev
}

// platformSeedKubeconfig hands the operator a kubeconfig of the seed with a one-hour token of the portal's
// service account (cluster-admin).
func (h *Handler) platformSeedKubeconfig(w http.ResponseWriter, r *http.Request) {
	h.breakGlass(w, r, "seed-kubeconfig", platformlife.BreakGlassKubeconfig)
}

// platformClusterKubeconfig hands the operator a kubeconfig of the platform cluster with a one-hour token of its
// break-glass service account: it reads (no Secret), forwards a port to ArgoCD and reads ArgoCD's initial admin
// password (platformlife.PlatformKubeconfig).
func (h *Handler) platformClusterKubeconfig(w http.ResponseWriter, r *http.Request) {
	h.breakGlass(w, r, "platform-kubeconfig", platformlife.PlatformKubeconfig)
}

// breakGlass answers one of the two kubeconfig downloads. The entry is audited before the kubeconfig goes out; one
// that cannot be written keeps it in, and without an audit service no token is asked for. No cache may keep the
// answer. A failure is answered with fixed text: the clusters' own words stay in.
func (h *Handler) breakGlass(w http.ResponseWriter, r *http.Request, operation string,
	mint func(context.Context, *platformlife.Installation, *platformlife.Secrets) (string, time.Time, error)) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if h.audit == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "The audit log is not available, so no kubeconfig is issued"))
		return
	}
	in, err := h.platform.Get(r.Context(), chi.URLParam(r, "id"))
	if platformStoreErr(w, err) {
		return
	}
	sec, err := h.platform.Secrets(r.Context(), in.ID)
	if httpx.WriteError(w, err) {
		return
	}
	kubeconfig, expires, err := mint(r.Context(), in, sec)
	switch {
	case errors.Is(err, platformlife.ErrNoSeed), errors.Is(err, platformlife.ErrNoCluster):
		platformStoreErr(w, err)
		return
	case errors.Is(err, platformlife.ErrWorkloadAPI):
		platformErr(w, http.StatusBadGateway, platformlife.CodeWorkloadUnreachable, "The platform cluster did not issue a one-hour token", nil)
		return
	case err != nil:
		platformErr(w, http.StatusBadGateway, platformlife.CodeSeedAPIUnreachable, "The seed did not issue a token", nil)
		return
	}
	if err := h.audit.Log(r.Context(), breakGlassEvent(r, in, operation, expires)); httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, map[string]any{"kubeconfig": kubeconfig, "expiresAt": expires.UTC()})
}

// platformSeedRotate replaces the stored seed credential with a new token Secret's and deletes the old Secret.
// Refused while a run is unfinished: the run holds the credential it loaded. A rotation that finished is audited as
// ROTATE_CREDENTIAL with the two Secret names; so is one that answered 502 after the new credential was stored, with
// outcome FAILURE.
func (h *Handler) platformSeedRotate(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	in, err := h.platform.Get(r.Context(), id)
	if platformStoreErr(w, err) {
		return
	}
	if run, err := h.platform.ActiveRun(r.Context(), id); httpx.WriteError(w, err) {
		return
	} else if run != nil {
		platformStoreErr(w, platformlife.ErrRunActive)
		return
	}
	sec, err := h.platform.Secrets(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	err = platformlife.RotateSeedCredential(r.Context(), in, sec, func(ctx context.Context, old, next, token string) error {
		return h.platform.SetSeedToken(ctx, id, old, next, token)
	})
	switch {
	case errors.Is(err, platformlife.ErrNoSeed), errors.Is(err, platformlife.ErrRunActive), errors.Is(err, platformlife.ErrRevConflict),
		errors.Is(err, platformlife.ErrNoEncryptionKey), errors.Is(err, platformlife.ErrNotFound):
		platformStoreErr(w, err)
		return
	case err != nil: // fixed text: the seed's own words stay in
		// The store may have committed before the step that failed: that change is audited too (best effort; a read
		// that fails here leaves no entry).
		if after, gerr := h.platform.Get(r.Context(), id); gerr == nil && h.audit != nil {
			if ev, ok := seedRotateUnfinishedEvent(r, in, after); ok {
				h.audit.LogEvent(ev)
			}
		}
		platformErr(w, http.StatusBadGateway, platformlife.CodeSeedAPIUnreachable,
			"The rotation did not finish. status.seed.tokenSecret names the credential that is stored; rotate again to delete the older token Secrets.", nil)
		return
	}
	after, err := h.platform.Get(r.Context(), id)
	if platformStoreErr(w, err) {
		return
	}
	// The rotation's own entry (the audit middleware skips this route: its row would read CREATE and say nothing).
	// Written after the fact and never a reason to fail: the old token is already revoked.
	if h.audit != nil {
		h.audit.LogEvent(seedRotateEvent(r, after, in.Status.Seed.TokenSecret, after.Status.Seed.TokenSecret))
	}
	v, err := h.platformView(r, after)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, v)
}

// PlatformRegister creates or adopts one provider row of a managed-services installation from a stage 10 body and
// answers its id. A row is the installation's by config.provider plus config.platformInstallationId; on an adopted
// row only the keys stage 10 owns and the kubeconfig are rewritten. The list and the insert run in one transaction
// under the installation's run lock, so two runners cannot both find no row and both create one. A body the provider
// validation refuses is ErrProviderConflict: no retry changes it.
func (h *Handler) PlatformRegister(ctx context.Context, body map[string]any) (id string, err error) {
	cfg, _ := body["config"].(map[string]any)
	installation, _ := cfg[platformlife.ProviderMarker].(string)
	err = h.repo.db.WithTx(ctx, func(ctx context.Context) error {
		if err := h.repo.db.LockXact(ctx, "platform-run:"+installation); err != nil {
			return err
		}
		rows, err := h.repo.ListRaw(ctx, externalServiceCollection)
		if err != nil {
			return err
		}
		row, err := platformlife.AdoptProvider(rows, body)
		if err != nil {
			return err
		}
		if row == nil {
			row, err = h.createExternalService(ctx, body)
			var refused *httpx.HTTPError
			if errors.As(err, &refused) && refused.Status >= 400 && refused.Status < 500 {
				return fmt.Errorf("%w: %s", platformlife.ErrProviderConflict, refused.Msg)
			}
			id, _ = row["_id"].(string)
			return err
		}
		secret, _ := body["secret"].(map[string]any)
		kubeconfig, _ := secret["kubeconfig"].(string)
		setSecretLeaf(h, ensureMap(row, "secret"), "kubeconfig", kubeconfig)
		id, _ = row["_id"].(string)
		return h.repo.ReplaceDoc(ctx, externalServiceCollection, id, row)
	})
	return id, err
}

// PlatformDisable sets the provider rows of an installation DISABLED (Remove), each with a note of the status it had,
// which the next install's stage 10 gives back (platformlife.DisableProvider, AdoptProvider).
func (h *Handler) PlatformDisable(ctx context.Context, installationID string) error {
	rows, err := h.repo.ListRaw(ctx, externalServiceCollection)
	if err != nil {
		return err
	}
	for _, row := range rows {
		cfg, _ := row["config"].(map[string]any)
		if cfg[platformlife.ProviderMarker] != installationID || !platformlife.DisableProvider(row) {
			continue
		}
		id, _ := row["_id"].(string)
		if err := h.repo.ReplaceDoc(ctx, externalServiceCollection, id, row); err != nil {
			return err
		}
	}
	return nil
}
