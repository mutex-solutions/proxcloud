package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type Handler struct {
	svc     *Service
	policy  *Policy
	orgSvc  *org.Service
	users   *user.Repo
	audit   *audit.Service
	billing *billing.Repo
	cloud   *cloud.Repo
	esSvc   *externalservice.Service
	// pricing + engine back the client price-preview (/pricing/{projectId}/service/{esId}[/types]):
	// rate a BillingResource against the project's price plans.
	pricing *pricing.Repo
	engine  *pricing.Engine
	// metadata reads the admin-configured instanceMetadataOption collection for the create-form
	// metadata panel.
	metadata *InstanceMetadataReader
	// defaultRegion is the region assumed when a project binding names none
	// (cfg.DefaultRegion).
	defaultRegion string
	// downloads mints/resolves short-lived cloud-object download tokens (object-store DOWNLOAD →
	// GET /download/{token}); apiBaseURL builds the token URL. Set via SetDownloads (nil-safe).
	downloads  *cloud.DownloadRepo
	apiBaseURL string
	// customMenu feeds admin Custom Menu items into the /init menu (nil-safe;
	// merged into the menu build). Set via SetCustomMenu.
	customMenu *CustomMenuReader
	// cephCreds stores/reads per-project Ceph RGW (S3) credentials — required to build a project-keyed
	// ceph client for bucket + object I/O, and written by the ceph bootstrap. Set via SetCephCreds
	// (nil = ceph-s3 provisioning + writes are unavailable).
	cephCreds *cephcred.Repo
	// cephKeys stores the project's EXTRA S3 access keys (each is its own RGW user, granted onto buckets
	// via bucket policy). Set via SetCephCreds (nil = the S3-key surface is unavailable).
	cephKeys *cephcred.KeyRepo
	// kamajiFor builds the Managed-Kubernetes service for a kamaji provider (management-cluster
	// client from the provider's kubeconfig). Set via SetKamaji (nil = kamaji provisioning +
	// cluster writes are unavailable).
	kamajiFor func(es *externalservice.ExternalService) (*kamaji.Service, error)
	// dbaasFor builds the Managed-Database service for a dbaas provider (DB-cluster client from
	// the provider's kubeconfig). Set via SetDbaas (nil = dbaas provisioning + database writes
	// are unavailable).
	dbaasFor func(es *externalservice.ExternalService) (*dbaas.Service, error)
	// pveFor builds the Proxmox VE service for a proxmox provider (admin-token client).
	// Set via SetProxmox (nil = proxmox provisioning + VM writes are unavailable).
	pveFor func(es *externalservice.ExternalService) (*pve.Service, error)
	// pveFlavors is the Proxmox flavor catalog (SetPVEFlavors; nil = proxmox SERVER
	// creates are unavailable).
	pveFlavors *pveflavor.Repo
	// sdnLock serializes PVE SDN applies platform-wide (SetSDNLock; nil = proxmox
	// network writes are unavailable).
	sdnLock *lock.ShedLock
	// publicIPs allocates Proxmox public-NIC addresses (SetPublicIPs; nil = public
	// NICs cannot attach).
	publicIPs *PublicIPRepo
	// seeds holds Proxmox server seeds (SetSeeds; nil = user-data is refused).
	seeds *SeedRepo
	// csiGW gives new Kubernetes clusters block volumes through the CSI gateway (SetCSIGateway; nil = the tenant-side
	// controller, offered only with csiSharedStores).
	csiGW *CSIGateway
	// sandboxFor builds the sandbox service for a sandbox provider (sandbox-cluster client from
	// the provider's kubeconfig). Always nil until phase SB3 adds its setter and mounts the
	// SANDBOX legs of cloud_sandbox.go; until then no route reaches them.
	sandboxFor func(es *externalservice.ExternalService) (*sandbox.Service, error)
}

// SetCustomMenu wires the customMenuItem reader into the /init menu build.
func (h *Handler) SetCustomMenu(r *CustomMenuReader) { h.customMenu = r }

// SetCephCreds wires the ceph credential + S3-key stores (enables ceph-s3 provisioning, bucket writes and
// the per-project S3 access keys).
func (h *Handler) SetCephCreds(r *cephcred.Repo, keys *cephcred.KeyRepo) {
	h.cephCreds, h.cephKeys = r, keys
}

// SetKamaji wires the kamaji Managed-Kubernetes service factory (enables kamaji provisioning and
// the KUBERNETES_CLUSTER write surface for kamaji providers).
func (h *Handler) SetKamaji(f func(es *externalservice.ExternalService) (*kamaji.Service, error)) {
	h.kamajiFor = f
}

// SetDbaas wires the dbaas Managed-Database service factory (enables dbaas provisioning and the
// DATABASE_CLUSTER write surface for dbaas providers).
func (h *Handler) SetDbaas(f func(es *externalservice.ExternalService) (*dbaas.Service, error)) {
	h.dbaasFor = f
}

// SetProxmox wires the Proxmox VE service factory (enables proxmox provisioning and the
// SERVER/VOLUME/IMAGE write surface for proxmox providers).
func (h *Handler) SetProxmox(f func(es *externalservice.ExternalService) (*pve.Service, error)) {
	h.pveFor = f
}

// SetDownloads wires the cloud-download token repo + the api base URL for the object DOWNLOAD action.
func (h *Handler) SetDownloads(d *cloud.DownloadRepo, apiBaseURL string) {
	h.downloads, h.apiBaseURL = d, apiBaseURL
}

func NewHandler(svc *Service, policy *Policy, orgSvc *org.Service, users *user.Repo, a *audit.Service, billingRepo *billing.Repo, cloudRepo *cloud.Repo, esSvc *externalservice.Service, pricingRepo *pricing.Repo, metadata *InstanceMetadataReader, defaultRegion string) *Handler {
	return &Handler{svc: svc, policy: policy, orgSvc: orgSvc, users: users, audit: a, billing: billingRepo, cloud: cloudRepo, esSvc: esSvc, pricing: pricingRepo, engine: pricing.NewEngine(nil), metadata: metadata, defaultRegion: defaultRegion}
}

// projectAudit emits a PROJECT event from a client (clientUser) action.
func (h *Handler) projectAudit(u *user.User, p *Project, action string) {
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextProject
	ev.Action = action
	ev.ResourceType = audit.ResourceProject
	ev.ResourceID = p.ID
	ev.ResourceDisplayName = p.Name
	ev.OrganizationID = p.OrganizationID
	ev.ProjectID = p.ID
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

// cloudResourceAudit emits a client cloud-resource event that carries the RESOURCE's own
// identity (kind, id, name) rather than only the project's — so the event is findable by
// searching for the bucket/server/etc. The coarse action (CLOUD_RESOURCE_CREATE/DELETE/ACTION)
// stays for backward-compatible filtering; the specific verb (e.g. MAKE_BUCKET_PUBLIC, REBOOT)
// goes in resourceMetadata. project/org ids still scope the event for the org-audit reader.
// A nil cr degrades to a project-scoped event (never drops the audit).
func (h *Handler) cloudResourceAudit(u *user.User, p *Project, action, verb string, cr *cloud.CloudResource) {
	if cr == nil {
		h.projectAudit(u, p, action)
		return
	}
	h.audit.LogAsync(newCloudResourceEvent(u, p, action, verb, cr))
}

// unauditedActions change nothing and hand out no credential. GET_KUBECONFIG and GET_CONNECTION_INFO are reads too, but
// who took a credential is what an audit trail is for.
var unauditedActions = map[string]bool{"GET_SETTINGS": true, "GET_POLICY": true, "GET_LOGS": true, "LIST_BACKUPS": true}

// runAudited runs a provider's action handler and logs once it handled the action and succeeded, so a handler with many
// actions needs no audit call on each success path. The event never carries the action's data: much of it is secret.
func runAudited(w http.ResponseWriter, protoMajor int, action string, handle func(http.ResponseWriter) bool, log func()) bool {
	ww := middleware.NewWrapResponseWriter(w, protoMajor)
	handled := handle(ww)
	if handled && ww.Status()/100 == 2 && !unauditedActions[action] {
		log()
	}
	return handled
}

// newCloudResourceEvent builds the cloud-resource audit event (pure — no I/O, unit-testable).
func newCloudResourceEvent(u *user.User, p *Project, action, verb string, cr *cloud.CloudResource) audit.AuditEvent {
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextProject
	ev.Action = action
	ev.ResourceType = cloudAuditKind(cr.Type)
	ev.ResourceID = cloudAuditID(cr)
	ev.ResourceDisplayName = cloudResourceName(cr)
	ev.OrganizationID = p.OrganizationID
	ev.ProjectID = p.ID
	meta := map[string]any{"projectName": p.Name, "resourceType": cr.Type, "cacheId": cr.ID}
	if cr.ExternalID != "" {
		meta["externalId"] = cr.ExternalID
	}
	if verb != "" && verb != action {
		meta["verb"] = verb
	}
	ev.ResourceMetadata = meta
	ev.Outcome = audit.OutcomeSuccess
	return ev
}

// cloudAuditKind maps a cloud resource type to an audit resourceType, defaulting to a generic
// CLOUD_RESOURCE for types with no dedicated audit constant.
func cloudAuditKind(t string) string {
	if t == "" {
		return "CLOUD_RESOURCE"
	}
	return t
}

// cloudAuditID prefers the provider-side external id (PVE vmid / RGW name — what an operator sees in the cloud) and
// falls back to the proxcloud cache id.
func cloudAuditID(cr *cloud.CloudResource) string {
	if cr.ExternalID != "" {
		return cr.ExternalID
	}
	return cr.ID
}

// cloudResourceName is the human name of a cloud resource for the audit display column:
// bucketName for buckets, else name/displayName from Data, else the external id.
func cloudResourceName(cr *cloud.CloudResource) string {
	for _, k := range []string{"bucketName", "name", "displayName"} {
		if s, ok := cr.Data[k].(string); ok && s != "" {
			return s
		}
	}
	if cr.ExternalID != "" {
		return cr.ExternalID
	}
	return cr.ID
}

// Routes registers the project endpoints under the /api/v1 group (the platform
// subset; cloud/billing-usage endpoints are deferred).
func (h *Handler) Routes(r chi.Router) {
	r.Get("/project", h.list)
	r.Post("/project", h.create)
	r.Get("/project/{id}", h.get)
	r.Delete("/project/{id}", h.scheduleDeletion)
	r.Delete("/project/{id}/now", h.deleteNow)
	r.Delete("/project/{id}/cancel", h.cancelDeletion)
	r.Post("/project/{id}/rename", h.rename)
	r.Put("/project/{id}/organization", h.updateOrganization)
	r.Get("/project/{id}/members", h.getMembers)
	r.Post("/project/{id}/members", h.addUserToProject)
	r.Put("/project/{id}/members/{sub}/role", h.updateProjectMemberRole)
	r.Delete("/project/{id}/members", h.removeUserFromProject)
	r.Get("/project/{id}/billing", h.billingSummary)
	// POST /{id}/billing/{billingProfileId} → swap the project's billing profile
	// (validity-gated on both profiles).
	r.Post("/project/{id}/billing/{billingProfileId}", h.changeBillingProfile)
	r.Get("/project/{id}/resources/counts", h.resourceCounts)
	r.Get("/project/{id}/resource/count", h.resourceStats)
	r.Get("/project/{id}/quota-usage", h.quotaUsage)
	r.Get("/project/{id}/gpu-capacity", h.projectGPUCapacity)
	r.Get("/project/{id}/resource-types", h.resourceTypes)
	// GET /{id}/public-networks → the provider's router:external networks filtered by the
	// project's publicNetworkIds allow-list — see publicnetworks.go.
	r.Get("/project/{id}/public-networks", h.publicNetworks)
	r.Get("/project/{id}/instance-metadata-options", h.instanceMetadataOptions)
	// Price-preview (create forms) — empty when billingResources is empty (the
	// wizard sends []) or the rating preview is deferred; 200-empty, not 404, so the FE renders.
	r.Post("/pricing/{projectId}/service/{externalServiceId}", h.pricingSingle)
	r.Post("/pricing/{projectId}/service/{externalServiceId}/types", h.pricingTypes)
	// Client cloud read/glue surface (dashboard bootstrap) — see clientcloud.go.
	r.Get("/init/{projectId}", h.initUI)
	r.Get("/search/{projectId}", h.search)
	r.Get("/project/{projectId}/service", h.projectServices)
	// Service get (by service id). chi: the position-3 param reuses the sibling
	// {serviceType} node name; the handler reads it as the service id.
	r.Get("/project/{projectId}/service/{serviceType}", h.projectServiceByID)
	r.Get("/project/{projectId}/service/{serviceType}/location", h.projectLocations)
	r.Get("/project/{id}/cost-info", h.projectCostInfo)
	r.Post("/project/{id}/init", h.projectInit)
	// POST /{id}/resource?type= → cached resources filtered by type.
	r.Post("/project/{id}/resource", h.cloudResourceList)
	// GET /{projectId}/cloud/{id} → one cached resource (live-refreshed).
	r.Get("/project/{id}/cloud/{resourceId}", h.cloudGet)
	// cloud writes (project:cloud_resource:manage gated; INTERNAL network only).
	r.Post("/project/{id}/cloud", h.cloudCreate)
	r.Delete("/project/{id}/cloud/{resourceId}", h.cloudDelete)
	r.Post("/project/{id}/cloud/{resourceId}/action", h.cloudAction)
	// POST /{id}/cloud/{resourceId}/upload-bucket-file
	// ?objectName= with the raw file body → stream into the bucket (204).
	r.Post("/project/{id}/cloud/{resourceId}/upload-bucket-file", h.cloudUploadBucketFile)
	// Download: GET /download/{token} streams a cloud object (whitelisted; token=auth).
	r.Get("/download/{token}", h.cloudObjectDownload)
	// NoCloud seed of a server (whitelisted; the per-VM token is the auth).
	r.Get("/nocloud/{token}/{file}", h.nocloudFile)
	// Console: GET /console/{token}/websockify reverse-proxies the console WebSocket to the
	// PVE node (whitelisted; token=auth — a bearer can't ride a WS handshake).
	// Keeps the PVE API off the public internet.
	r.Get("/console/{token}/websockify", h.cloudConsoleProxy)
	// Collection-level action: catalog actions for the create wizard
	// (PUBLIC_IMAGES, flavor catalog) — must precede no static segment so it doesn't
	// collide with /{resourceId}/action (chi: static "action" wins over the {resourceId} param).
	r.Post("/project/{id}/cloud/action", h.cloudBulkAction)
	// ceph-s3 per-project S3 access keys ("IAM keys"): the project's own credentials, plus extra keys
	// that can be granted onto individual buckets. These return/rotate live secrets, so each handler gates
	// on project:cloud_resource:api_access (NOT :manage) — see cloud_bucket_settings.go.
	r.Get("/project/{id}/s3-credentials", h.s3Credentials)
	r.Post("/project/{id}/s3-credentials/rotate", h.s3CredentialsRotate)
	r.Get("/project/{id}/s3-keys", h.s3KeyList)
	r.Post("/project/{id}/s3-keys", h.s3KeyCreate)
	r.Post("/project/{id}/s3-keys/{keyId}/rotate", h.s3KeyRotate)
	r.Delete("/project/{id}/s3-keys/{keyId}", h.s3KeyDelete)
}

// billingSummary returns the project's billing-profile summary. Project-membership-gated via
// GetProject; the profile id is the project's own (if set) else the owning org's.
func (h *Handler) billingSummary(w http.ResponseWriter, r *http.Request) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	bpID := p.BillingProfileID
	if bpID == "" {
		o, err := h.orgSvc.FindOrganization(r.Context(), p.OrganizationID)
		if err != nil {
			h.fail(w, err)
			return
		}
		if o == nil {
			h.fail(w, httpx.NotFound("Organization not found"))
			return
		}
		bpID = o.BillingProfileID
	}
	bp, err := h.billing.FindByID(r.Context(), bpID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if bp == nil {
		h.fail(w, httpx.NotFound("Billing profile not found"))
		return
	}
	httpx.OK(w, billing.ToSummary(bp).WithFinancials(r.Context(), h.billing, time.Now().UTC()))
}

// changeBillingProfile handles POST /{id}/billing/{bpId}: PROJECT_UPDATE gate, then BOTH profiles
// must be valid for changing (negative balance or SUSPENDED → 400). The current profile is the
// project's own billingProfileId by-id 404 (no org fallback); the target resolves through the
// getBillingProfile(sub,id) read gate (bp 404 → org 404 → BILLING_PROFILE_READ 403). Then the
// project status is synced (NEW → DISABLED) + the billingProfileId swap.
// A move to a profile that cannot pay disables the project, and is refused while it holds anything billable (holdForProfile), so
// there is nothing to suspend here.
func (h *Handler) changeBillingProfile(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectUpdate)
	if !ok {
		return
	}
	if err := CheckTransition(ActionChangeBilling, proj.Status); err != nil {
		h.fail(w, err)
		return
	}
	now := time.Now().UTC()
	bal := billing.NewBalanceService(h.billing)
	// Current profile = the project's own, else the owning org's (the raw-field read 404s/500s
	// on the blank field greenfield projects carry, so the effective-profile fallback is used).
	curID := proj.BillingProfileID
	if curID == "" {
		if o, err := h.orgSvc.FindOrganization(r.Context(), proj.OrganizationID); err == nil && o != nil {
			curID = o.BillingProfileID
		}
	}
	cur, err := h.billing.FindByID(r.Context(), curID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if cur == nil {
		h.fail(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", curID)))
		return
	}
	if err := profileValidForChanging(r.Context(), bal, cur, now); err != nil {
		h.fail(w, err)
		return
	}
	// Target profile through the getBillingProfile(sub,id) gate.
	bpID := chi.URLParam(r, "billingProfileId")
	target, err := h.billing.FindByID(r.Context(), bpID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if target == nil {
		h.fail(w, httpx.NotFound(fmt.Sprintf("Billing profile with id %s not found. ", bpID)))
		return
	}
	// The target profile must belong to the project's OWN organization — a profile from any other
	// org must never be attachable (cross-org billing hijack, even with read on that other org).
	if !sameOrgBillingProfile(target, proj) {
		h.fail(w, httpx.Forbidden("The billing profile does not belong to this project's organization"))
		return
	}
	o, err := h.orgSvc.FindOrganization(r.Context(), target.OrganizationID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if o == nil {
		h.fail(w, httpx.NotFound("Billing Profile has no organization associated"))
		return
	}
	// Attaching a profile mutates the project's billing — gate on the stronger update permission,
	// not the read gate.
	if err := h.policy.RequireOrgPermission(r.Context(), u.Sub, o.ID, rbac.BillingProfileUpdate); err != nil {
		h.fail(w, err)
		return
	}
	if err := profileValidForChanging(r.Context(), bal, target, now); err != nil {
		h.fail(w, err)
		return
	}
	// Nothing here enables a project: this is a member's action. A DISABLED one comes up when its profile activates, or on the
	// next open once it is on a profile that pays.
	expect := proj.Status
	if err := h.holdForProfile(r.Context(), proj, target); err != nil {
		h.fail(w, err)
		return
	}
	proj.BillingProfileID = target.ID
	if _, err := h.svc.SaveIfStatus(r.Context(), proj, expect); err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, *proj)
}

// holdForProfile disables the project when what it is moving to cannot pay: a profile not active yet, or none at all (nil).
// Metering and rating cover only ENABLED projects, so what it holds there would go unbilled. Pausing it from here would be a
// tenant's own billing change freezing its workloads, and activation has no leg to resume them, so the move is refused until
// nothing billable is left. With billing unconfigured profiles stay NEW for good and nothing is rated anywhere: nothing to hold.
func (h *Handler) holdForProfile(ctx context.Context, proj *Project, target *billing.BillingProfile) error {
	if target != nil && target.Status != billing.StatusNew && target.Status != billing.StatusSuspended {
		return nil
	}
	_, _, configured, err := h.billing.Configuration(ctx)
	if err != nil {
		return err
	}
	if !configured {
		return nil
	}
	what, err := h.billableResource(ctx, proj)
	if err != nil {
		return err
	}
	if what != "" {
		why, fix := "This billing profile is not active yet", "activate the profile"
		if target == nil {
			why, fix = "That organization has no billing profile", "add one"
		}
		return httpx.NewError(http.StatusConflict, http.StatusConflict,
			why+", and "+what+" in this project would go unbilled. Delete it first, or "+fix)
	}
	proj.Status = StatusDisabled
	return nil
}

// billableRows is every row type the charge job rates (the billing registry in cmd/api), with the words the refusal uses. Storage
// counts as much as what runs: S3 keys already issued keep working against the store while the project sits DISABLED.
var billableRows = []struct{ rowType, what string }{
	{cloud.TypeKubernetesCluster, "a Kubernetes cluster"},
	{cloud.TypeDatabaseCluster, "a managed database"},
	{cloud.TypeServer, "a server"},
	{cloud.TypeVolume, "a volume"},
	{cloud.TypeBucket, "a bucket"},
	{cloud.TypeShare, "a file share"},
	{cloud.TypeFloatingIP, "a public IP"},
	{cloud.TypeImage, "an image"},
}

// billableResource names one thing the project holds that billing would rate, "" when there is none. Live for VMs as well as
// from rows: a row can lag a start, and not knowing refuses the change. A provider that has been removed has nothing left to ask:
// the rows above still count what it left. A provider that is there and does not answer is not knowing.
func (h *Handler) billableResource(ctx context.Context, proj *Project) (string, error) {
	for _, b := range billableRows {
		rows, err := h.cloud.FindByProjectAndType(ctx, proj.ID, b.rowType)
		if err != nil {
			return "", err
		}
		if len(rows) > 0 {
			return b.what, nil
		}
	}
	if h.pveFor == nil {
		return "", nil
	}
	for _, id := range proj.ServiceIDs() {
		if proj.ServiceProvider(id) != "proxmox" {
			continue
		}
		es, err := h.esSvc.Get(ctx, id)
		if errors.Is(err, externalservice.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		ps, err := h.pveFor(es)
		if err != nil {
			return "", billableUnknown(proj, id, err)
		}
		rows, err := ps.C.ClusterResources(ctx, "vm")
		if err != nil {
			return "", billableUnknown(proj, id, err)
		}
		pool := ps.PoolFor(proj.ID)
		for _, r := range rows {
			if r.Pool == pool && r.Status != "stopped" && r.Template != 1 {
				return "server " + r.Name, nil
			}
		}
	}
	return "", nil
}

// billableUnknown is the refusal when a provider cannot say what the project runs on it. The cause goes to the log, not the tenant.
func billableUnknown(proj *Project, serviceID string, cause error) error {
	slog.Error("project move: read provider", "project", proj.ID, "service", serviceID, "err", cause)
	return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
		"A provider of this project cannot be reached, so what the project holds there is not known. Try again later")
}

// HoldForOperatorMove is the hold on an operator's move of an ENABLED or DISABLED project (own profile and organization as they
// will be afterwards): what the client paths apply through holdForProfile, without their refusals about leaving a profile, which
// are the operator's to overrule. True means the move has to disable the project. A move that leaves the paying profile as it is
// holds nothing.
func (h *Handler) HoldForOperatorMove(ctx context.Context, projectID, ownProfileID, organizationID string) (bool, error) {
	proj, err := h.svc.GetProjectByID(ctx, projectID)
	if err != nil {
		return false, err
	}
	enabled := proj.IsEnabled()
	if !enabled && !proj.IsDisabled() {
		return false, nil
	}
	cur, err := h.paidByProfile(ctx, proj)
	if err != nil {
		return false, err
	}
	next, err := h.paidByProfile(ctx, &Project{BillingProfileID: ownProfileID, OrganizationID: organizationID})
	if err != nil {
		return false, err
	}
	if cur == nil && next == nil || cur != nil && next != nil && cur.ID == next.ID {
		return false, nil
	}
	if err := h.holdForProfile(ctx, proj, next); err != nil {
		return false, err
	}
	return enabled && proj.IsDisabled(), nil
}

// sameOrgBillingProfile reports whether a target billing profile belongs to the project's own
// organization — the cross-org attach guard for changeBillingProfile.
func sameOrgBillingProfile(target *billing.BillingProfile, proj *Project) bool {
	return target != nil && proj != nil && target.OrganizationID == proj.OrganizationID
}

// paidByProfile is the profile that pays for the project: its own, else the organization's. Nil when it has none.
func (h *Handler) paidByProfile(ctx context.Context, proj *Project) (*billing.BillingProfile, error) {
	id := proj.BillingProfileID
	if id == "" {
		o, err := h.orgSvc.FindOrganization(ctx, proj.OrganizationID)
		if err != nil || o == nil {
			return nil, err
		}
		id = o.BillingProfileID
	}
	if id == "" {
		return nil, nil
	}
	return h.billing.FindByID(ctx, id)
}

// profileValidForChanging: current balance < 0 or a SUSPENDED profile blocks the change with the
// translated message (trailing space included).
func profileValidForChanging(ctx context.Context, bal *billing.BalanceService, bp *billing.BillingProfile, now time.Time) error {
	b, err := bal.CurrentBalance(ctx, bp.ID, now)
	if err != nil {
		return err
	}
	if b.IsNegative() || bp.Status == billing.StatusSuspended {
		return httpx.BadRequest("The billing profile is not valid for changing. ")
	}
	return nil
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	httpx.WriteError(w, err)
}

// principal loads the initialized User (400 if not).
func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (*user.User, bool) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	return u, true
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	views, err := h.svc.ListForSub(r.Context(), u.Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.List(w, views)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	var req struct {
		Name           string   `json:"name"`
		OrganizationID string   `json:"organizationId"`
		MemberSubs     []string `json:"memberSubs"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	o, err := h.orgSvc.GetOrganizationForUser(r.Context(), req.OrganizationID, u.Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := h.policy.RequireOrgPermission(r.Context(), u.Sub, o.ID, rbac.ProjectCreate); err != nil {
		h.fail(w, err)
		return
	}
	p, err := h.svc.CreateProject(r.Context(), u.Sub, req.Name, o.ID, req.MemberSubs)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.projectAudit(u, p, audit.ActionCreate)
	httpx.OK(w, *p)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, *p)
}

// project loads a project the user is a member of and enforces a project-level
// permission, returning the project on success. Centralizes the gate shared by
// the mutating endpoints. On failure it has already written the response.
func (h *Handler) project(w http.ResponseWriter, r *http.Request, u *user.User, permKey string) (*Project, bool) {
	p, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	if err := h.policy.RequireProjectPermission(r.Context(), u.Sub, p, permKey); err != nil {
		h.fail(w, err)
		return nil, false
	}
	return p, true
}

func (h *Handler) scheduleDeletion(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectDelete)
	if !ok {
		return
	}
	p, err := h.svc.ScheduleDeletion(r.Context(), proj, r.URL.Query().Get("cascade") == "true")
	if err != nil {
		h.fail(w, err)
		return
	}
	h.projectAudit(u, p, audit.ActionDelete)
	_, _ = h.users.DeleteCustomInfo(r.Context(), u.Sub, "lastProject")
	httpx.OK(w, *p)
}

func (h *Handler) deleteNow(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectDelete)
	if !ok {
		return
	}
	p, err := h.svc.DeleteNow(r.Context(), proj.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	_, _ = h.users.DeleteCustomInfo(r.Context(), u.Sub, "lastProject")
	httpx.OK(w, *p)
}

func (h *Handler) cancelDeletion(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectUpdate)
	if !ok {
		return
	}
	p, err := h.svc.CancelDeletion(r.Context(), proj.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	_, _ = h.users.DeleteCustomInfo(r.Context(), u.Sub, "lastProject")
	httpx.OK(w, *p)
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectUpdate)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	p, err := h.svc.Rename(r.Context(), u.Sub, proj.ID, req.Name)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.projectAudit(u, p, audit.ActionUpdate)
	httpx.OK(w, *p)
}

func (h *Handler) updateOrganization(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectUpdate)
	if !ok {
		return
	}
	var req struct {
		OrganizationID string `json:"organizationId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	// Validate the user is a member of the TARGET org.
	target, err := h.orgSvc.GetOrganizationForUser(r.Context(), req.OrganizationID, u.Sub)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := CheckTransition(ActionChangeBilling, proj.Status); err != nil {
		h.fail(w, err)
		return
	}
	// A move changes who pays, so it passes the checks of changeBillingProfile: a project cannot leave a suspended or
	// indebted profile this way. It then pays through the target organization, as a profile of another organization may not
	// stay attached.
	now := time.Now().UTC()
	bal := billing.NewBalanceService(h.billing)
	cur, err := h.paidByProfile(r.Context(), proj)
	if err != nil {
		h.fail(w, err)
		return
	}
	if cur != nil {
		if err := profileValidForChanging(r.Context(), bal, cur, now); err != nil {
			h.fail(w, err)
			return
		}
	}
	expect := proj.Status
	var next *billing.BillingProfile // stays nil for an organization with no profile, or a deleted one
	if target.BillingProfileID != "" {
		if next, err = h.billing.FindByID(r.Context(), target.BillingProfileID); err != nil {
			h.fail(w, err)
			return
		}
	}
	if next != nil {
		if err := profileValidForChanging(r.Context(), bal, next, now); err != nil {
			h.fail(w, err)
			return
		}
	}
	if err := h.holdForProfile(r.Context(), proj, next); err != nil {
		h.fail(w, err)
		return
	}
	proj.OrganizationID = target.ID
	proj.BillingProfileID = ""
	if _, err := h.svc.SaveIfStatus(r.Context(), proj, expect); err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, *proj)
}

func (h *Handler) getMembers(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	// Membership check only (no extra permission gate).
	proj, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	members, err := h.svc.Members(r.Context(), proj)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.List(w, members)
}

func (h *Handler) addUserToProject(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectManageMembers)
	if !ok {
		return
	}
	var req struct {
		UserSub string `json:"userSub"`
		Role    string `json:"role"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	p, err := h.svc.AddMember(r.Context(), proj.ID, req.UserSub, req.Role)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, *p)
}

func (h *Handler) removeUserFromProject(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectManageMembers)
	if !ok {
		return
	}
	p, err := h.svc.RemoveMember(r.Context(), proj.ID, r.URL.Query().Get("sub"))
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, *p)
}

func (h *Handler) updateProjectMemberRole(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectManageMembers)
	if !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	p, err := h.svc.UpdateMemberRole(r.Context(), proj.ID, chi.URLParam(r, "sub"), req.Role)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, *p)
}
