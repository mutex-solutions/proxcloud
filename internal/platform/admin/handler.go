package admin

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/catalog"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/paging"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/sso"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// collectChecker is the operator's way out of a charge whose outcome is unknown (payment.TransactionScanner).
type collectChecker interface {
	RecheckCollect(ctx context.Context, id string) (*pricing.CollectTransaction, error)
	CloseCollect(ctx context.Context, id string) (*pricing.CollectTransaction, error)
}

// txnRefunder is the narrow interface the admin refund + bank-transfer endpoints need
// (refund + bank-transfer add-funds) — kept as an interface so admin
// does not depend on the whole payment package.
type txnRefunder interface {
	RefundFunds(ctx context.Context, txnID string) (*billing.AccountCreditTransaction, error)
	// ProcessBankTransfer settles/void an AddFunds txn per the resolved bankTransfer status
	// (APPROVED→credit the account, REJECTED→txn FAILED w/ comments as gatewayMessage).
	ProcessBankTransfer(ctx context.Context, txnID, bankStatus, comments string) (*billing.AccountCreditTransaction, error)
	// ProcessAddFunds re-drives the gateway confirm for a txn (Stripe PI retrieve / BankTransfer
	// doc dispatch) — the admin sync-transaction endpoint is literally this call.
	ProcessAddFunds(ctx context.Context, txnID string) (*billing.AccountCreditTransaction, error)
}

// Handler serves the /api/v1/admin/** surface. Authorization follows the composite
// admin-dashboard authorization strategy:
//   - REMOTE_OIDC (adminIssuer set, the deployed posture): the token MUST come from the admin
//     issuer (Keycloak master) AND carry the admin client id in azp; such a user is
//     auto-provisioned SUPER_ADMIN → all admin permissions. Anything else → 403.
//   - LOCAL_IDP (adminIssuer empty): fall back to the adminPermission-doc role/permission check.
type Handler struct {
	repo          *Repo
	catalog       *catalog.Repo
	users         *user.Repo
	refund        txnRefunder
	collects      collectChecker
	pricing       *pricing.Repo
	billing       *billing.Repo
	cloud         *cloud.Repo
	platformcfg   *platformconfig.Repo
	audit         *audit.Service
	esSvc         *externalservice.Service                                        // cloud-admin: load+decrypt externalService for live cloud reads
	defaultRegion string                                                          // fallback region (cfg.DefaultRegion)
	pveFor        func(es *externalservice.ExternalService) (*pve.Service, error) // SetProxmox; nil in tests → pve reads degrade to empty
	adminIssuer   string
	adminClientID string
	activation    *billing.ActivationService // bp activate/suspend/resume orchestration (nil → 501)
	projectCloud  *ProjectCloudOps           // live per-project cloud legs (nil → those endpoints stay 501)
	// inviteToProject invites a user to a project (admin user-create projectIds loop).
	inviteToProject func(ctx context.Context, u *user.User, email, projectID string) error
	// keycloak makes and removes logins for operator-created users (SetKeycloak; nil = Create
	// user refuses, the users of an external IdP appear at their first login).
	keycloak *sso.KCAdmin
	// kamajiFor builds the Managed-Kubernetes service for a kamaji provider (nil in tests →
	// the cluster chart-pin endpoints answer 501).
	kamajiFor func(es *externalservice.ExternalService) (*kamaji.Service, error)
	// csiMigrate moves a cluster onto the CSI gateway (nil → 501).
	csiMigrate CSIMigrator
	// pveFlavors is the Proxmox flavor catalog (nil in tests → the surface 503s).
	pveFlavors *pveflavor.Repo
	pay        *billing.PayService // bill record-payment (nil → 501)
	// dbaasFor builds the Managed-Database service for a dbaas provider (nil in tests → the
	// database chart-pin endpoints answer 501).
	dbaasFor func(es *externalservice.ExternalService) (*dbaas.Service, error)
	hmacEnc  *textcrypt.Encryptor // seals the secrets of generated hmac keys
	// sandboxGates checks a sandbox provider's release gates (SetSandboxGates; nil = none can go live).
	sandboxGates         func(ctx context.Context, es *externalservice.ExternalService) []sandbox.Gate
	seedArtifacts        *pve.SeedArtifactRepo // secret-free operator cleanup status
	pveIncidents         chi.Router            // operator-only DB observation reads; nil until explicitly wired
	mailOutbox           mailOutboxReader
	validationOutbox     ValidationApprovalOutbox
	validationCanceler   validationCanceler
	validationActivation validationActivationTail
	durableStore         *durablejobs.Store
	durableHistory       *durablejobs.History
	durableValidate      func(*durablejobs.Job) error
}

// SetHmacEncryptor wires the data-at-rest key for hmac key secrets.
func (h *Handler) SetHmacEncryptor(enc *textcrypt.Encryptor) { h.hmacEnc = enc }

// SetKamaji wires the kamaji Managed-Kubernetes service factory (the admin chart-pin surface).
// SetPayService wires the bill PayService (record-payment; carries the bill_is_paid mail +
// suspension auto-resume hooks set in cmd/api). nil → the endpoint answers 501.
func (h *Handler) SetPayService(p *billing.PayService) { h.pay = p }

// SetProxmox wires the pve service factory for the cloud-admin live reads.
func (h *Handler) SetProxmox(f func(es *externalservice.ExternalService) (*pve.Service, error)) {
	h.pveFor = f
}

func (h *Handler) SetKamaji(f func(es *externalservice.ExternalService) (*kamaji.Service, error)) {
	h.kamajiFor = f
}

// SetDbaas wires the dbaas Managed-Database service factory (the admin chart-pin surface).
func (h *Handler) SetDbaas(f func(es *externalservice.ExternalService) (*dbaas.Service, error)) {
	h.dbaasFor = f
}

// SetActivation wires the billing ActivationService (built in cmd/api with the cloud
// suspender + notifier + project-bootstrap legs).
func (h *Handler) SetActivation(a *billing.ActivationService) {
	h.activation = a
	if a == nil {
		h.validationActivation = nil
	} else {
		h.validationActivation = a
	}
}

// ProjectCloudOps are the live per-project cloud legs wired from cmd/api for the ProjectAdmin /
// CloudResourceAdmin mutations (the per-project cloud legs). Nil ops (or a nil struct) leave the
// corresponding endpoints as 501 (tests construct the Handler without them).
type ProjectCloudOps struct {
	// PauseServers suspends/resumes a project's cached PVE VMs (best-effort per server).
	PauseServers func(ctx context.Context, projectID string, pause bool) error
	// Sync runs the live resource sync for one project, optionally scoped to one service.
	Sync func(ctx context.Context, projectID, serviceID string) error
	// Bootstrap provisions a project onto an external service — create-or-reuse the provider-side
	// tenant (PVE pool, ceph user, …) + attach the ProjectExternalService entry.
	Bootstrap func(ctx context.Context, projectID, esID string) error
	// CanDelete is the live pre-check gating a project-deletion schedule (project resolves / tenant
	// reachable). nil → the endpoint stays 501.
	CanDelete func(ctx context.Context, projectID string) error
	// DeleteNow flags a project for immediate deletion; the deletion job's next sweep tears it down. nil → 501.
	DeleteNow func(ctx context.Context, projectID string) error
	// HoldForMove answers whether a move of an ENABLED project onto this own profile and organization has to disable it, and refuses
	// one that would leave what it holds unbilled. nil → no hold.
	HoldForMove func(ctx context.Context, projectID, ownProfileID, organizationID string) (bool, error)
}

// SetProjectCloudOps wires the live project-cloud legs (cmd/api only).
func (h *Handler) SetProjectCloudOps(ops *ProjectCloudOps) { h.projectCloud = ops }

// SetKeycloak wires the login management behind admin user create and delete.
func (h *Handler) SetKeycloak(k *sso.KCAdmin) { h.keycloak = k }

// SetInviteToProject wires inviteToProject for the admin user-create
// projectIds loop (invites are best-effort there — each is caught and logged). nil → skipped.
func (h *Handler) SetInviteToProject(f func(ctx context.Context, u *user.User, email, projectID string) error) {
	h.inviteToProject = f
}

func NewHandler(repo *Repo, catalogRepo *catalog.Repo, users *user.Repo, refund txnRefunder, pricingRepo *pricing.Repo, billingRepo *billing.Repo, cloudRepo *cloud.Repo, platformcfgRepo *platformconfig.Repo, auditSvc *audit.Service, esSvc *externalservice.Service, defaultRegion string, adminIssuer, adminClientID string) *Handler {
	return &Handler{repo: repo, catalog: catalogRepo, users: users, refund: refund, pricing: pricingRepo, billing: billingRepo, cloud: cloudRepo, platformcfg: platformcfgRepo, audit: auditSvc, esSvc: esSvc, defaultRegion: defaultRegion, adminIssuer: adminIssuer, adminClientID: adminClientID}
}

func (h *Handler) Routes(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		// Global admin audit trail: auto-emit an ADMIN_AREA/PLATFORM event after every successful
		// (2xx) POST/PUT/DELETE under /admin (see audit_mw.go).
		r.Use(h.auditMiddleware)
		r.Get("/me", h.me)
		if h.pveIncidents != nil {
			r.Mount("/service/{id}/incidents", h.pveIncidents)
		}
		r.Get("/mail-outbox", h.mailOutboxOverview)
		r.Get("/mail-outbox/{id}", h.mailOutboxStatus)
		r.Get("/durable-jobs", h.durableStatus)
		r.Post("/durable-jobs/{id}/replay", h.durableReplay)
		r.Get("/flavor-categories", h.flavorCategories)
		r.Get("/flavor-categories/{id}", h.rawByID("admin:flavor_category:manage", "flavorCategory", "id",
			func(string) *httpx.HTTPError { return httpx.NotFound("Flavor category not found") }))
		// Transaction reads by paymentGatewayId + the live-gateway sync (not wired) — see txn_reads.go.
		r.Get("/account-credit-transactions/{id}/payment-gateway", h.txnByGateway("accountCreditTransaction"))
		r.Get("/account-credit-transactions/{id}/sync", h.accountCreditTxnSync)
		r.Get("/collect-transactions/{id}/payment-gateway", h.txnByGateway("collectTransaction"))
		r.Get("/collect-transactions/{id}/sync", h.collectTxnSync)
		r.Post("/collect-transactions/{id}/close", h.collectTxnClose)
		r.Get("/credit-card-transaction/{id}/payment-gateway", h.txnByGateway("creditCardTransaction"))
		// The global cursor-paginated audit log (all
		// request interfaces; the org/account readers are scoped). Gated ADMIN_AUDIT_READ.
		r.Get("/audit", h.auditList)
		r.Get("/audit/export", h.auditExport)
		// Empty-state admin list reads (populated DTO shaping deferred). The key matters only on
		// the LOCAL_IDP path (REMOTE_OIDC auto-grants SUPER_ADMIN).
		r.Get("/promotion-codes", h.listRaw("admin:promotional_credit:manage", "promotionCode"))
		// integrations + hmac-keys lists go through secret-stripping handlers (NEVER leak credentials).
		r.Get("/integrations", h.integrationsList)
		r.Get("/hmac-keys", h.hmacKeysList)
		// Custom menu items (full CRUD + reorder + placeholders) — see custommenu.go.
		h.routeCustomMenu(r)
		// Image categories; empty under greenfield.
		r.Get("/images/categories", h.listRaw("admin:image_group:manage", "imageCategory"))
		// Instance metadata options; empty under greenfield.
		r.Get("/instance-metadata-options", h.listRaw("admin:instance_metadata:manage", "instanceMetadataOption"))
		// Price plans; empty under greenfield (pricing not deployed).
		r.Get("/price-plan", h.listRaw("admin:price_plan:read", "pricePlan"))
		// Refund a deposit's Stripe payment.
		r.Post("/account-credit-transactions/refund/{id}", h.refundTransaction)
		// The client Permission metadata (key/description/resourceType) —
		// deterministic, gated ADMIN_ROLE_READ.
		r.Get("/permissions", h.permissions)
		// Tax reads (TaxRate config; RO fixture on both datastores → comparison-testable).
		r.Get("/tax", h.taxList)
		r.Get("/tax/{id}", h.taxByID)
		// Transaction admin by-id reads (empty under greenfield → 404-path; reuse mappers).
		r.Get("/account-credit-transactions/{id}", h.accountCreditTxnByID)
		r.Get("/collect-transactions/{id}", h.collectTxnByID)
		r.Get("/credit-card-transaction/{id}", h.creditCardTxnByID)
		// Transaction by-billing-profile lists (ALL statuses, createdAt DESC; empty-state).
		r.Get("/account-credit-transactions/{billingProfileId}/billing-profile", h.accountCreditTxnByBP)
		r.Get("/collect-transactions/{billingProfileId}/billing-profile", h.collectTxnByBP)
		r.Get("/credit-card-transaction/{billingProfileId}/billing-profile", h.creditCardTxnByBP)
		// Static country + currency lists (PUBLIC, whitelisted in pkg/auth; no admin gate).
		// Drive the country + Base-Currency dropdowns.
		r.Get("/billing/configuration/countries", h.adminCountries)
		r.Get("/billing/configuration/currencies", h.adminCurrencies)
		// The 58 admin-permission {key,description} metadata entries
		// (deterministic; gated ADMIN_PERMISSION_READ).
		r.Get("/admin-permissions/available-permissions", h.availablePermissions)
		// Bank-transfer reads (raw BankTransfer domain; empty under greenfield).
		r.Get("/bank-transfer", h.bankTransferList)
		r.Get("/bank-transfer/{id}", h.bankTransferByID)
		// Cloud-resource reads. by-id → lookup (null → empty {}, NOT 404); the
		// by-user / by-project list endpoints (empty under greenfield → {data:[],paging}).
		r.Get("/cloud-resource/{id}", h.cloudResourceByID)
		r.Get("/cloud-resource/user/{userId}", h.cloudResourcesByUser)
		r.Get("/cloud-resource/project/{projectId}", h.cloudResourcesByProject)
		// The suspension processes for a billing profile
		// (empty under greenfield → {data:[],paging}; populated raw-domain DTO deferred).
		r.Get("/suspensions/{billingProfileId}", h.suspensionsByBP)
		// The 5 built-in admin roles (deterministic; custom adminRole-collection
		// roles are appended but empty under greenfield).
		r.Get("/admin-roles", h.adminRoles)
		// External resource providers (empty list).
		r.Get("/external-resource-providers", h.externalResourceProviders)
		// Onboarding status — PUBLIC (whitelisted in pkg/auth); fully-onboarded
		// (platform+billing config seeded, REMOTE_OIDC) → 404 "Onboarding already completed".
		r.Get("/onboarding/status", h.onboardingStatus)
		// By-id reads whose typed DTO is deferred — empty greenfield → the exact 404/400 path
		// (raw-domain happy path fails loud, billing-list precedent). Perms + messages
		// match the admin endpoints (oracle-probed).
		r.Get("/project/{id}", h.rawByID("admin:project:read", "project", "id",
			func(id string) *httpx.HTTPError {
				return httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", id))
			}))
		r.Get("/organizations/{id}", h.organizationByID)
		r.Get("/promotion-codes/{id}", h.rawByID("admin:promotional_credit:manage", "promotionCode", "id",
			func(string) *httpx.HTTPError { return httpx.NotFound("Promotion code not found") }))
		r.Get("/promotional-credits/{id}", h.rawByID("admin:account_credit:read", "promotionalCredit", "id",
			func(string) *httpx.HTTPError { return httpx.BadRequest("Promotional credit not found") }))
		r.Get("/savings-plans/{id}", h.rawByID("admin:savings_plan:read", "savingsPlan", "id",
			func(string) *httpx.HTTPError { return httpx.NotFound("Savings plan not found") }))
		r.Get("/savings-contracts/{id}", h.rawByID("admin:savings_plan:read", "savingsContract", "id",
			func(string) *httpx.HTTPError { return httpx.NotFound("Savings contract not found") }))
		r.Get("/price-plan/{id}", h.rawByID("admin:price_plan:read", "pricePlan", "id",
			func(id string) *httpx.HTTPError {
				return httpx.NotFound(fmt.Sprintf("Could not find price plan with id %s", id))
			}))
		r.Get("/price-plan/rule/{id}", h.rawByID("admin:price_plan:read", "pricePlanRule", "id",
			func(string) *httpx.HTTPError { return httpx.NotFound("PricePlanRule not found. ") }))
		// Organization by-billing-profile / by-member (empty greenfield → []).
		r.Get("/organizations/by-billing-profile/{billingProfileId}", h.orgsByBillingProfile)
		r.Get("/organizations/by-member/{sub}", h.orgsByMember)
		// Project by-user / by-organization / by-billing-profile / external-services
		// (empty → []). `external-services` is a STATIC sibling of the {id} param (no chi conflict).
		r.Get("/project/by-user", h.projectsByUser)
		r.Get("/project/by-organization", h.projectsByOrganization)
		r.Get("/project/{billingProfileId}/billing-profile", h.projectsByBillingProfile)
		r.Get("/project/external-services/{externalServiceId}", h.projectsByExternalService)
		// Billing-configuration reads (the seeded default config; fixture on both datastores).
		r.Get("/billing/configuration", h.billingConfigList)
		r.Get("/billing/configuration/current", h.billingConfigCurrent)
		r.Get("/billing/configuration/{id}", h.billingConfigByID)
		// External-service reads (base path /admin/service). Empty greenfield → []/404.
		r.Get("/service", h.serviceList)
		r.Get("/service/{id}", h.serviceByID)
		r.Get("/service/{id}/project/services", h.projectServices)
		r.Get("/service/{id}/user/services", h.userServices)
		// ExternalService live provider reads (cloud-admin) — see cloudadmin.go (routeCloudAdmin):
		// /service/proxmox/auth, /service[/{id}]/os-images, /service/{id}/{os-flavors,volume/types,
		// gpu-info}, /service/regions, /cloud-resource/public-networks/{id}.
		h.routeCloudAdmin(r)
		r.Get("/service/{id}/holder-operations/{project}", h.holderOperationStatus)
		r.Post("/service/{id}/holder-operations/{project}/poll", h.holderOperationPoll)
		h.routePVEFlavors(r)
		// Admin init — gated (any admin, no specific permission); branding from the
		// seeded platformConfiguration + REMOTE_OIDC strategies.
		r.Get("/init", h.adminInit)
		// Admin dashboard landing reads (the FE queries these on load). Bare lists are raw-domain
		// passthrough (empty under greenfield → []; populated DTO shaping deferred). Stats = live
		// counts + empty insight buckets.
		r.Get("/stats", h.adminStats)
		r.Get("/organizations", h.organizationList)
		r.Get("/project", h.projectAdminList)
		r.Get("/user", h.listRaw("admin:user:read", "users"))
		r.Get("/billing-profile", h.billingProfileAdminList)
		r.Get("/bill", h.billAdminList)
		// Cloud-resource bare list = a bare {data:[...]} (NO paging, unlike the others).
		r.Get("/cloud-resource", h.cloudResourcesAll)
		// Remaining admin-menu bare lists (raw-domain passthrough; empty/seeded under greenfield).
		r.Get("/admin-permissions", h.listRaw("admin:permission:read", "adminPermission"))
		r.Get("/message-templates", h.listRaw("admin:message_template:read", "messageTemplate"))
		r.Get("/pdf-templates", h.listRaw("admin:message_template:read", "pdfTemplate"))
		r.Get("/savings-plans", h.listRaw("admin:savings_plan:read", "savingsPlan"))
		r.Get("/savings-contracts", h.listRaw("admin:savings_plan:read", "savingsContract"))
		// Third-party integration reads (stats = static catalog; by-id/type/category).
		r.Get("/integrations/stats", h.integrationStats)
		r.Get("/integrations/type/{type}", h.integrationsByType)
		r.Get("/integrations/category/{category}", h.integrationsByCategory)
		r.Get("/integrations/{id}", h.integrationByID)
		// Platform-configuration reads (the seeded default config; fixture on both datastores).
		r.Get("/platform-configuration", h.platformConfigList)
		h.routePlatformConfigMut(r) // POST / PUT/{id} / DELETE/{id} / PUT/{id}/regions
		r.Get("/platform-configuration/current", h.platformConfigCurrent)
		// The login configuration — static sibling of {id}; MUST be
		// registered (else it falls into platformConfigByID → always-500). REMOTE_OIDC greenfield:
		// no oauth2 login clients, localIdp off.
		r.Get("/platform-configuration/login-config", h.platformLoginConfig)
		r.Get("/platform-configuration/{id}", h.platformConfigByID)

		// ── Admin mutations — per-controller route groups, see <controller>.go ──
		h.routeMessageTemplate(r)
		h.routePDFTemplate(r)
		h.routePromotionCode(r)
		h.routePromotionalCredit(r)
		h.routeAccountCredit(r)
		h.routeSavingsContract(r)
		h.routeSavingsPlan(r)
		h.routeAdminRole(r)
		h.routeAdminPermission(r)
		h.routeBankTransfer(r)
		h.routeThirdPartyIntegration(r)
		h.routeBillingConfigMut(r)

		// ── Admin mutations/reads — billing/pricing/transactions ──
		h.routeBill(r)
		h.routeBillingProfile(r)
		h.routePricePlan(r)
		h.routePriceAdjustmentRule(r)
		h.routeTax(r)
		h.routeTransaction(r)
		h.routeTransactionMut(r)
		h.routeBuiltInInvoice(r)

		// ── Admin mutations/reads — org/user/onboarding/hmac/metadata ──
		h.routeOrganization(r)
		h.routeOrganizationRole(r)
		h.routeUser(r)
		h.routeUserManagement(r)
		// Onboarding MUTATIONS (platform/billing-configuration, initialize-user) are intentionally
		// NOT registered: the public first-run wizard has no consumer here
		// (ProxCloud is REMOTE_OIDC-only so there is no first-local-admin to bootstrap, config is
		// applied by deploy/seed, and the FE never calls it). Removing the routes closes the
		// unauthenticated setup-tampering surface entirely. The status GET stays (registered above).
		h.routeHmacKey(r)
		h.routeInstanceMetadata(r)

		// ── Admin mutations — cloud/service/project ──
		h.routeExtResourceProvider(r)
		h.routeImageGroup(r)
		h.routeFlavorCategory(r)
		h.routeCloudResourceMut(r)
		h.routeProjectMut(r)
		h.routeProjectManager(r)
		h.routeExternalServiceMut(r)
	})
}

// adminCountries returns the static country list.
// PUBLIC — the security configuration permits this path (mirrored in pkg/auth publicExact),
// so there is no admin gate (no token/rc on this request).
func (h *Handler) adminCountries(w http.ResponseWriter, r *http.Request) {
	httpx.List(w, billing.Countries())
}

// adminCurrencies returns the currency list
// (deduped by currencyCode). PUBLIC — whitelisted in pkg/auth (no admin gate). Drives the
// Base Currency dropdown on the billing-configuration Settings tab.
func (h *Handler) adminCurrencies(w http.ResponseWriter, r *http.Request) {
	httpx.List(w, billing.Currencies())
}

func (h *Handler) accountCreditTxnByBP(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	txs, err := h.billing.AllAccountCreditTransactionsByProfile(r.Context(), chi.URLParam(r, "billingProfileId"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, billing.AccountCreditTransactionsToDtos(txs))
}

func (h *Handler) collectTxnByBP(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	txs, err := h.billing.AllCollectTransactionsByProfile(r.Context(), chi.URLParam(r, "billingProfileId"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, billing.CollectTransactionsToDtos(txs))
}

func (h *Handler) creditCardTxnByBP(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	txs, err := h.billing.CreditCardTransactionsByProfile(r.Context(), chi.URLParam(r, "billingProfileId"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, billing.CreditCardTransactionsToDtos(txs))
}

// accountCreditTxnByID handles the account-credit transaction by-id read (ADMIN_TRANSACTION_READ).
func (h *Handler) accountCreditTxnByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	id := chi.URLParam(r, "id")
	txn, err := h.billing.AccountCreditTransactionByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if txn == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Transaction %s not found ", id)))
		return
	}
	httpx.OK(w, billing.AccountCreditTransactionToDto(txn))
}

// SetCollectChecker wires the charge re-check and close routes.
func (h *Handler) SetCollectChecker(c collectChecker) { h.collects = c }

// collectTxnAction runs one operator action on a charge and answers with the row (ADMIN_TRANSACTION_MANAGE).
func (h *Handler) collectTxnAction(w http.ResponseWriter, r *http.Request, act func(context.Context, string) (*pricing.CollectTransaction, error)) {
	if !h.require(w, r, "admin:transaction:manage") {
		return
	}
	id := chi.URLParam(r, "id")
	txn, err := act(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if txn == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Bill transaction with id %s not found ", id)))
		return
	}
	httpx.OK(w, billing.CollectTransactionToDto(txn))
}

// collectTxnSync asks the gateway about a charge, past the scanner's window too, like a deposit's /sync. It is a GET for
// the same reason as that one: the re-check only records what the gateway says.
func (h *Handler) collectTxnSync(w http.ResponseWriter, r *http.Request) {
	h.collectTxnAction(w, r, h.collects.RecheckCollect)
}

// collectTxnClose ends a PENDING charge the gateway says did not succeed, or one with no gateway reference once the
// operator found no charge at the gateway.
func (h *Handler) collectTxnClose(w http.ResponseWriter, r *http.Request) {
	h.collectTxnAction(w, r, h.collects.CloseCollect)
}

// collectTxnByID handles the collect transaction by-id read (ADMIN_TRANSACTION_READ).
func (h *Handler) collectTxnByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	id := chi.URLParam(r, "id")
	txn, err := h.billing.CollectTransactionByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if txn == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Bill transaction with id %s not found ", id)))
		return
	}
	httpx.OK(w, billing.CollectTransactionToDto(txn))
}

// creditCardTxnByID handles the credit-card transaction by-id read (ADMIN_TRANSACTION_READ).
func (h *Handler) creditCardTxnByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	id := chi.URLParam(r, "id")
	txn, err := h.billing.CreditCardTransactionByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if txn == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Credit card transaction with id %s not found ", id)))
		return
	}
	httpx.OK(w, billing.CreditCardTransactionToDto(txn))
}

// taxList returns all tax rates (gated ADMIN_TAX_READ). The pricing.TaxRate
// json shape carries whole-percent levels and dates (nulls omitted).
func (h *Handler) taxList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:tax:read") {
		return
	}
	rates, err := h.pricing.AllTaxRates(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	if rates == nil {
		rates = []pricing.TaxRate{}
	}
	httpx.List(w, rates)
}

// taxByID returns a single tax rate (gated ADMIN_TAX_READ), or 404.
func (h *Handler) taxByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:tax:read") {
		return
	}
	id := chi.URLParam(r, "id")
	rates, err := h.pricing.AllTaxRates(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	for i := range rates {
		if rates[i].ID == id {
			httpx.OK(w, rates[i])
			return
		}
	}
	httpx.WriteError(w, httpx.NotFound("Tax rate not found"))
}

// permissions returns the full Permission metadata set.
func (h *Handler) permissions(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:role:read") {
		return
	}
	httpx.List(w, rbac.AllPermissionMeta())
}

// refundTransaction handles
// POST /api/v1/admin/account-credit-transactions/refund/{id}, gated ADMIN_TRANSACTION_MANAGE. Full-
// refunds a SUCCESS deposit's PaymentIntent and voids its AccountCredit (→ REFUNDED).
func (h *Handler) refundTransaction(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:manage") {
		return
	}
	if h.refund == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError, "refund not configured"))
		return
	}
	txn, err := h.refund.RefundFunds(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, billing.AccountCreditTransactionToDto(txn))
}

// adminContext resolves the caller's admin role + granted permission patterns.
// ok=false → not an admin (→ 403).
func (h *Handler) adminContext(r *http.Request) (role string, granted []string, ok bool) {
	rc := httpx.RC(r.Context())
	// A verified Admin-API hmac key (SigV4) is an operator credential: it already holds the
	// whole /admin-api/v1 surface and the operator job triggers. Granting SUPER_ADMIN here
	// lets the MCP admin toolset (Bearer pk.sk) drive the full admin surface too.
	if rc.SigV4KeyID != "" {
		return "SUPER_ADMIN", []string{"admin:*"}, true
	}
	if h.adminIssuer != "" {
		// REMOTE_OIDC: admin issuer + admin client in azp → auto-provisioned SUPER_ADMIN.
		if rc.Issuer != h.adminIssuer || (h.adminClientID != "" && rc.Azp != h.adminClientID) {
			return "", nil, false
		}
		return "SUPER_ADMIN", []string{"admin:*"}, true
	}
	// LOCAL_IDP: adminPermission doc → role → patterns.
	ap, err := h.repo.FindBySub(r.Context(), rc.Sub)
	if err != nil || ap == nil {
		return "", nil, false
	}
	role = ap.Role
	if role == "" {
		role = "SUPER_ADMIN"
	}
	return role, rolePermissions(role), true
}

// require gates on an admin permission key; returns false (and writes a 403) if denied.
func (h *Handler) require(w http.ResponseWriter, r *http.Request, key string) bool {
	_, granted, ok := h.adminContext(r)
	if !ok || !rbac.Matches(granted, key) {
		httpx.WriteError(w, httpx.Forbidden("You do not have the required permission: "+key))
		return false
	}
	return true
}

// Require is the permission gate for handlers mounted outside this package.
func (h *Handler) Require(w http.ResponseWriter, r *http.Request, key string) bool {
	return h.require(w, r, key)
}

// AdminProfileDto is the admin-profile response (null email/names omitted).
type AdminProfileDto struct {
	Sub         string   `json:"sub,omitempty"`
	Email       string   `json:"email,omitempty"`
	FirstName   string   `json:"firstName,omitempty"`
	LastName    string   `json:"lastName,omitempty"`
	Role        string   `json:"role,omitempty"`
	Pending     bool     `json:"pending"` // primitive — always emitted; /me never sets it → false
	Permissions []string `json:"permissions"`
}

// me returns the caller's admin profile: sub/role + expanded permission keys
// (+ the User's email/names when a User doc exists; omitted otherwise, e.g. the master admin).
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	role, granted, ok := h.adminContext(r)
	if !ok {
		httpx.WriteError(w, httpx.Forbidden("User does not have admin access"))
		return
	}
	rc := httpx.RC(r.Context())
	dto := AdminProfileDto{Sub: rc.Sub, Role: role, Permissions: ExpandPatterns(granted)}
	if u, err := h.users.FindBySub(r.Context(), rc.Sub); err == nil && u != nil {
		dto.Email, dto.FirstName, dto.LastName = u.Email, u.FirstName, u.LastName
	}
	httpx.OK(w, dto)
}

// listRaw builds an admin-gated handler that lists a collection (empty → []).
func (h *Handler) listRaw(key, collection string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.require(w, r, key) {
			return
		}
		pg, ok := paging.FromRequest(w, r)
		if !ok {
			return
		}
		// Shape each doc to the API JSON the domains serialize (_id→id, drop _class). The Go
		// domains never emit _id/_class, so a raw passthrough diverges + breaks the admin UI's
		// id-keyed row tracking / detail links.
		if pg.Active {
			items, total, err := h.repo.ListRawPage(r.Context(), collection, pg)
			if httpx.WriteError(w, err) {
				return
			}
			for i := range items {
				shapeDoc(items[i])
			}
			httpx.Page(w, items, paging.OffsetPaging(pg, total))
			return
		}
		items, err := h.repo.ListRaw(r.Context(), collection)
		if httpx.WriteError(w, err) {
			return
		}
		for i := range items {
			shapeDoc(items[i])
		}
		httpx.List(w, items)
	}
}

// AdminRoleDto is a role element in the roles list: id==name for a
// built-in role, its granted patterns, the expanded key set, and builtIn=true.
type AdminRoleDto struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Permissions         []string `json:"permissions"`
	ExpandedPermissions []string `json:"expandedPermissions"`
	BuiltIn             bool     `json:"builtIn"`
}

// adminRoles returns the roles (ADMIN_ROLE_READ): the 5 built-in roles PLUS
// the custom roles persisted in the `adminRole` collection (created via POST /admin-roles from the
// admin UI). Both are emitted as customAdminRoleDto (a superset of AdminRoleDto's fields).
func (h *Handler) adminRoles(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:role:read") {
		return
	}
	names := []string{"SUPER_ADMIN", "ADMIN", "SUPPORT", "BILLING_ADMIN", "VIEWER"}
	roles := make([]customAdminRoleDto, 0, len(names)+2)
	for _, n := range names {
		p := rolePermissions(n)
		roles = append(roles, customAdminRoleDto{
			ID: n, Name: n, Permissions: p, ExpandedPermissions: ExpandPatterns(p), BuiltIn: true,
		})
	}
	custom, err := h.repo.ListRaw(r.Context(), adminRoleCollection)
	if httpx.WriteError(w, err) {
		return
	}
	for _, doc := range custom {
		roles = append(roles, adminRoleDtoFromDoc(doc))
	}
	httpx.List(w, roles)
}

// externalResourceProviders lists the external resource providers
// (ADMIN_SERVICE_READ): all providers, or filtered by ?externalServiceId (empty → []).
func (h *Handler) externalResourceProviders(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	items, err := h.repo.ListExternalResourceProviders(r.Context(), r.URL.Query().Get("externalServiceId"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}

// OnboardingStatusResponse is the onboarding-status response (only the 200 branch, never hit
// in the seeded env — fully-onboarded always 404s).
type OnboardingStatusResponse struct {
	PlatformConfigurationExists   bool   `json:"platformConfigurationExists"`
	BillingConfigurationExists    bool   `json:"billingConfigurationExists"`
	AuthStrategy                  string `json:"authStrategy"`
	LocalAdminNeedsInitialization bool   `json:"localAdminNeedsInitialization"`
}

// onboardingStatus reports the onboarding status — PUBLIC (no admin gate; whitelisted).
// fullyOnboarded = platformConfigExists && billingConfigExists && !localAdminNeedsInitialization →
// 404 "Onboarding already completed". Greenfield is REMOTE_OIDC, so
// localAdminNeedsInitialization is always false (the LOCAL_IDP no-user/no-admin bootstrap N/A).
func (h *Handler) onboardingStatus(w http.ResponseWriter, r *http.Request) {
	pc, err := h.repo.CountDocs(r.Context(), "platformConfiguration")
	if httpx.WriteError(w, err) {
		return
	}
	bc, err := h.repo.CountDocs(r.Context(), "billingConfiguration")
	if httpx.WriteError(w, err) {
		return
	}
	status := OnboardingStatusResponse{PlatformConfigurationExists: pc > 0, BillingConfigurationExists: bc > 0, AuthStrategy: "REMOTE_OIDC"}
	if status.PlatformConfigurationExists && status.BillingConfigurationExists && !status.LocalAdminNeedsInitialization {
		httpx.WriteError(w, httpx.NotFound("Onboarding already completed"))
		return
	}
	httpx.OK(w, status)
}

// rawByID builds an admin-gated by-id read over a collection: empty greenfield → the given 404/400
// error; a populated doc returns the raw document (typed DTO deferred, fails loud if populated).
func (h *Handler) rawByID(perm, collection, idParam string, errFn func(id string) *httpx.HTTPError) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.require(w, r, perm) {
			return
		}
		id := chi.URLParam(r, idParam)
		doc, err := h.repo.FindByIDRaw(r.Context(), collection, id)
		if httpx.WriteError(w, err) {
			return
		}
		if doc == nil {
			httpx.WriteError(w, errFn(id))
			return
		}
		// _id→id, drop _class — the domains serialize id, never _id/_class.
		httpx.OK(w, shapeDoc(doc))
	}
}

// shapeIntegration maps a thirdPartyIntegration doc to the ThirdPartyIntegrationDto shape: `_id`→`id`
// and DROP the encrypted `secret` — credentials (SMTP password / API keys) must NEVER reach the
// browser — plus the legacy `_class`. config /
// metadata / name / description / thirdParty pass through.
func shapeIntegration(d pgdoc.M) pgdoc.M {
	if d == nil {
		return nil
	}
	if v, ok := d["_id"]; ok {
		d["id"] = v
		delete(d, "_id")
	}
	delete(d, "secret")
	delete(d, "_class")
	return d
}

// hmacKeysList lists the hmac_keys collection with the `secretKey` stripped — the secret half of an
// HMAC key pair must NEVER reach the browser. Maps `_id`→`id`, drops `_class`. ADMIN_HMAC_KEY_MANAGE.
func (h *Handler) hmacKeysList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:hmac_key:manage") {
		return
	}
	items, err := h.repo.ListRaw(r.Context(), "hmac_keys")
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		if v, ok := items[i]["_id"]; ok {
			items[i]["id"] = v
			delete(items[i], "_id")
		}
		delete(items[i], "secretKey")
		delete(items[i], "_class")
	}
	httpx.List(w, items)
}

// integrationsList handles the integrations list: all installed integrations
// (secret stripped); ADMIN_INTEGRATION_READ.
func (h *Handler) integrationsList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:integration:read") {
		return
	}
	items, err := h.repo.ListRaw(r.Context(), "thirdPartyIntegration")
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeIntegration(items[i])
	}
	httpx.List(w, items)
}

// integrationStats returns the static
// integration catalog, each entry's `installed` = whether an integration with that name exists (so a
// configured SMTP/Stripe/etc. shows as installed and the dashboard's setup checklist marks it done).
// ADMIN_INTEGRATION_READ.
func (h *Handler) integrationStats(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:integration:read") {
		return
	}
	installed, err := h.repo.InstalledThirdParties(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]ThirdPartyStats, len(ThirdPartyCatalog))
	for i, e := range ThirdPartyCatalog {
		e.Installed = installed[e.Name] // whether an integration with that name exists
		out[i] = e
	}
	httpx.List(w, out)
}

// integrationsByType lists installed integrations of a given type
// (secret stripped); ADMIN_INTEGRATION_READ.
func (h *Handler) integrationsByType(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:integration:read") {
		return
	}
	items, err := h.repo.ListRawFiltered(r.Context(), "thirdPartyIntegration", map[string]any{"thirdParty": chi.URLParam(r, "type")})
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeIntegration(items[i])
	}
	httpx.List(w, items)
}

// integrationsByCategory lists integrations by category: the category → the catalog entries in it
// (by name) → the installed integrations whose `thirdParty` is one of those names. The
// thirdPartyIntegration doc has NO `categories` field — the category lives in the catalog — so we map
// category → names → docs. Secret stripped. ADMIN_INTEGRATION_READ.
// This drives the dashboard's "Configure a Mail Gateway" task (category=Mail → the SMTP integration).
func (h *Handler) integrationsByCategory(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:integration:read") {
		return
	}
	category := chi.URLParam(r, "category")
	seen := map[string]bool{}
	names := []string{}
	for _, e := range ThirdPartyCatalog {
		for _, c := range e.Categories {
			if c == category && !seen[e.Name] {
				seen[e.Name] = true
				names = append(names, e.Name)
			}
		}
	}
	items := []pgdoc.M{}
	if len(names) > 0 {
		got, err := h.repo.ListRawFiltered(r.Context(), "thirdPartyIntegration", pgdoc.M{"thirdParty": pgdoc.M{"$in": names}})
		if httpx.WriteError(w, err) {
			return
		}
		items = got
	}
	for i := range items {
		shapeIntegration(items[i])
	}
	httpx.List(w, items)
}

// integrationByID handles the integration by-id read → the integration (secret stripped),
// or 404 "ThirdParty Integration with id %s not found"; ADMIN_INTEGRATION_READ.
func (h *Handler) integrationByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:integration:read") {
		return
	}
	id := chi.URLParam(r, "id")
	doc, err := h.repo.FindByIDRaw(r.Context(), "thirdPartyIntegration", id)
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("ThirdParty Integration with id %s not found", id)))
		return
	}
	httpx.OK(w, shapeIntegration(doc))
}

// platformConfigList lists the platform configurations (ADMIN_PLATFORM_CONFIG_READ).
func (h *Handler) platformConfigList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:platform_config:read") {
		return
	}
	cfgs, err := h.platformcfg.AllAdminConfigurations(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, cfgs)
}

// platformConfigCurrent returns the current platform configuration: the seeded default config (the
// create branch never runs in the seeded env); ADMIN_PLATFORM_CONFIG_READ.
func (h *Handler) platformConfigCurrent(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:platform_config:read") {
		return
	}
	cfg, err := h.platformcfg.CurrentAdminConfiguration(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	if cfg == nil {
		httpx.Empty(w)
		return
	}
	httpx.OK(w, cfg)
}

// PlatformLoginConfigInfo is the login-config response. Greenfield
// REMOTE_OIDC: no oauth2 login clients, localIdp disabled. baseRedirectUrl (a per-deployment
// template) is omitted — it is informational and only used when oauth2 login clients exist.
type PlatformLoginConfigInfo struct {
	Oauth2Clients      []any  `json:"oauth2Clients"`
	AdminAuthStrategy  string `json:"adminAuthStrategy"`
	ClientAuthStrategy string `json:"clientAuthStrategy"`
	LocalIdpEnabled    bool   `json:"localIdpEnabled"`
	BaseRedirectURL    string `json:"baseRedirectUrl,omitempty"`
}

// platformLoginConfig returns the platform
// login configuration (the social/SSO oauth2 login clients + auth strategies). REMOTE_OIDC greenfield
// has no registered oauth2 login clients → empty list, localIdp off. (ADMIN_PLATFORM_CONFIG_READ.)
func (h *Handler) platformLoginConfig(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:platform_config:read") {
		return
	}
	httpx.OK(w, PlatformLoginConfigInfo{
		Oauth2Clients:      []any{},
		AdminAuthStrategy:  "REMOTE_OIDC",
		ClientAuthStrategy: "REMOTE_OIDC",
		LocalIdpEnabled:    false,
	})
}

// platformConfigByID returns a platform configuration by id: a missing id yields
// HTTP 500 "No configuration found with id %s" (happy-path
// by-id deferred — the seeded id is masked anyway); ADMIN_PLATFORM_CONFIG_READ.
func (h *Handler) platformConfigByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:platform_config:read") {
		return
	}
	id := chi.URLParam(r, "id")
	// Return the config for a VALID id, 500 only when absent. (Was a broken stub that ALWAYS 500'd —
	// even for the real id — so the admin config edit page's record-fetch / post-save refresh failed →
	// the save never showed.)
	cfg, err := h.platformcfg.ByIDAdminConfiguration(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if cfg == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError,
			fmt.Sprintf("No configuration found with id %s", id)))
		return
	}
	httpx.OK(w, cfg)
}

// serviceList handles the external-service list (ADMIN_SERVICE_READ); empty → [].
func (h *Handler) serviceList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	items, err := h.repo.ListRaw(r.Context(), "externalService")
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeExternalService(items[i])
	}
	httpx.List(w, items)
}

// shapeExternalService maps the raw externalService doc to the admin DTO shape the FE expects:
// rename `_id` → `id` (the FE keys navigation off `id`) and DROP `secret` — the secret holds the
// provider credentials and must NEVER reach the browser.
func shapeExternalService(d pgdoc.M) {
	if d == nil {
		return
	}
	if v, ok := d["_id"]; ok {
		d["id"] = v
		delete(d, "_id")
	}
	delete(d, "secret")
}

// serviceByID handles the external-service view (ADMIN_SERVICE_READ): the external service,
// or HTTP 400 with errors.code 404 "Cloud provider is not found. Please contact support." (the
// odd status/code split — replicated exactly).
func (h *Handler) serviceByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	doc, err := h.repo.FindByIDRaw(r.Context(), "externalService", chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusBadRequest, http.StatusNotFound, "Cloud provider is not found. Please contact support."))
		return
	}
	shapeExternalService(doc)
	httpx.OK(w, doc)
}

// projectServices lists a project's external services: resolves the
// project first → 404 "The project with id %s was not found. " when absent (happy-path deferred).
func (h *Handler) projectServices(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	id := chi.URLParam(r, "id")
	proj, err := h.repo.FindByIDRaw(r.Context(), "project", id)
	if httpx.WriteError(w, err) {
		return
	}
	if proj == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("The project with id %s was not found. ", id)))
		return
	}
	httpx.List(w, []any{})
}

// userServices lists a user's external services:
// resolves the user by ID → 404 "User with id %s not found " when
// absent (happy-path list deferred → empty).
func (h *Handler) userServices(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	id := chi.URLParam(r, "id")
	u, err := h.repo.FindByIDRaw(r.Context(), "users", id)
	if httpx.WriteError(w, err) {
		return
	}
	if u == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("User with id %s not found ", id)))
		return
	}
	httpx.List(w, []any{})
}

// AdminInitializer is the admin-init response: null darkLogo/theme/scripts/
// localIdpAccountConsoleUrl omitted; localIdpEnabled is a primitive (always emitted).
type AdminInitializer struct {
	ID                 string `json:"id,omitempty"`
	Logo               string `json:"logo,omitempty"`
	FaviconURL         string `json:"faviconUrl,omitempty"`
	Name               string `json:"name,omitempty"`
	AdminAuthStrategy  string `json:"adminAuthStrategy,omitempty"`
	ClientAuthStrategy string `json:"clientAuthStrategy,omitempty"`
	LocalIdpEnabled    bool   `json:"localIdpEnabled"`
}

// adminInit returns the admin init payload: branding from the default platformConfiguration +
// the auth strategies. Gated to any admin (no specific permission; non-admin → 403, no-token → 401
// from the RS filter). Greenfield is REMOTE_OIDC → localIdpEnabled false, localIdpAccountConsoleUrl nil.
func (h *Handler) adminInit(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.adminContext(r); !ok {
		httpx.WriteError(w, httpx.Forbidden("You do not have admin access"))
		return
	}
	id, name, logo, favicon, err := h.repo.DefaultPlatformBranding(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, AdminInitializer{
		ID: id, Logo: logo, FaviconURL: favicon, Name: name,
		AdminAuthStrategy: "REMOTE_OIDC", ClientAuthStrategy: "REMOTE_OIDC", LocalIdpEnabled: false,
	})
}

// orgsByBillingProfile lists organizations for a billing profile.
func (h *Handler) orgsByBillingProfile(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:organization:read") {
		return
	}
	items, err := h.repo.ListRawFiltered(r.Context(), "organization", map[string]any{"billingProfileId": chi.URLParam(r, "billingProfileId")})
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}

// orgsByMember lists organizations a user is a member of.
func (h *Handler) orgsByMember(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:organization:read") {
		return
	}
	items, err := h.repo.OrganizationsByMemberSub(r.Context(), chi.URLParam(r, "sub"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}

// projectsByUser lists a user's projects (required ?sub).
func (h *Handler) projectsByUser(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:project:read") {
		return
	}
	items, err := h.repo.ListRawFiltered(r.Context(), "project",
		pgdoc.M{"memberships": pgdoc.M{"$contains": pgdoc.M{"sub": r.URL.Query().Get("sub")}}})
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}

// projectsByOrganization lists an organization's projects (required ?organizationId).
func (h *Handler) projectsByOrganization(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:project:read") {
		return
	}
	items, err := h.repo.ListRawFiltered(r.Context(), "project", map[string]any{"organizationId": r.URL.Query().Get("organizationId")})
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}

// projectsByBillingProfile lists the projects that BILL against a profile. Billing resolves the
// EFFECTIVE profile as the project's own billingProfileId, falling back to the owning org's
// (project.resolveBillingProfileID) — and greenfield projects carry a BLANK own id — so matching the
// project field alone misses them: also include the projects of every org attached to this profile
// whose own billingProfileId is blank.
func (h *Handler) projectsByBillingProfile(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:project:read") {
		return
	}
	bpID := chi.URLParam(r, "billingProfileId")
	items, err := h.repo.ListRawFiltered(r.Context(), "project", map[string]any{"billingProfileId": bpID})
	if httpx.WriteError(w, err) {
		return
	}
	seen := make(map[string]bool, len(items))
	for _, p := range items {
		if id, _ := p["_id"].(string); id != "" {
			seen[id] = true
		}
	}
	orgs, err := h.repo.ListRawFiltered(r.Context(), "organization", map[string]any{"billingProfileId": bpID})
	if httpx.WriteError(w, err) {
		return
	}
	orgIDs := make([]any, 0, len(orgs))
	for i := range orgs {
		if orgID, _ := orgs[i]["_id"].(string); orgID != "" {
			orgIDs = append(orgIDs, orgID)
		}
	}
	if len(orgIDs) > 0 {
		// One query, predicate pushed down: org projects that bill HERE — own id blank/absent (the
		// org fallback) or explicitly this profile. Projects billed to a different profile never load.
		projs, err := h.repo.ListRawFiltered(r.Context(), "project", map[string]any{
			"organizationId": map[string]any{"$in": orgIDs},
			"$or": []any{
				map[string]any{"billingProfileId": map[string]any{"$exists": false}},
				map[string]any{"billingProfileId": ""},
				map[string]any{"billingProfileId": bpID},
			},
		})
		if httpx.WriteError(w, err) {
			return
		}
		for _, p := range projs {
			id, _ := p["_id"].(string)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			items = append(items, p)
		}
	}
	httpx.List(w, items)
}

// projectsByExternalService lists projects for an external service
// (GET /api/v1/admin/project/external-services/{externalServiceId}): the projects whose services
// include the external service id. Empty under greenfield → []. Gated ADMIN_PROJECT_READ.
func (h *Handler) projectsByExternalService(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:project:read") {
		return
	}
	items, err := h.repo.ListRawFiltered(r.Context(), "project",
		pgdoc.M{"services": pgdoc.M{"$contains": pgdoc.M{"serviceId": chi.URLParam(r, "externalServiceId")}}})
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}

// auditList returns the global cursor-paginated audit log
// (all request interfaces, unlike the org/account-scoped readers). Gated ADMIN_AUDIT_READ.
// Filters: requestInterface/organizationId/resourceType/actorId/action/outcome/from/to/search;
// the projectId/resourceId/eventContext filters are not yet modeled in
// audit.Filter (the FE's bare ?limit=50 load needs none of them).
func (h *Handler) auditList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:audit:read") {
		return
	}
	q := r.URL.Query()
	after, before := q.Get("after"), q.Get("before")
	if after != "" && before != "" {
		httpx.WriteError(w, httpx.BadRequest("Cannot specify both 'after' and 'before'"))
		return
	}
	f := auditFilterFromQuery(q)
	limit := audit.ParseLimit(q.Get("limit"))
	events, next, prev, err := h.audit.Query(r.Context(), f, after, before, limit)
	if httpx.WriteError(w, err) {
		return
	}
	// Each event is hydrated into an AuditEventDto {event, organization,
	// project, user} before responding — the admin UI table binds those nested refs. A bare-event
	// list renders blank rows.
	dtos, err := h.repo.HydrateAuditEvents(r.Context(), events)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.CursorList(w, dtos, limit, next, prev)
}

// billingConfigList handles the billing-config list (ADMIN_BILLING_CONFIG_READ).
func (h *Handler) billingConfigList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_config:read") {
		return
	}
	cfgs, err := h.billing.AllBillingConfigurations(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, cfgs)
}

// billingConfigCurrent returns the current billing configuration: the
// existing default config (billing IS created in the seeded env, so never the create branch).
func (h *Handler) billingConfigCurrent(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_config:read") {
		return
	}
	cfg, err := h.billing.CurrentBillingConfiguration(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	if cfg == nil {
		httpx.Empty(w)
		return
	}
	httpx.OK(w, cfg)
}

// billingConfigByID returns a billing configuration by id → the
// config, or 400 "Billing configuration not found " (trailing space) when absent.
func (h *Handler) billingConfigByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:billing_config:read") {
		return
	}
	cfg, err := h.billing.BillingConfigurationByID(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if cfg == nil {
		httpx.WriteError(w, httpx.BadRequest("Billing configuration not found "))
		return
	}
	httpx.OK(w, cfg)
}

// availablePermissions returns
// the full admin-permission {key,description} metadata (gated ADMIN_PERMISSION_READ).
func (h *Handler) availablePermissions(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:permission:read") {
		return
	}
	httpx.List(w, AdminPermissionMeta)
}

// bankTransferList lists bank transfers (ADMIN_TRANSACTION_READ):
// the bank transfers for an integration (required ?integrationId), newest first.
func (h *Handler) bankTransferList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	items, err := h.repo.ListBankTransfers(r.Context(), r.URL.Query().Get("integrationId"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}

// bankTransferByID returns a bank transfer (ADMIN_TRANSACTION_READ):
// the bank transfer, or 404 "Bank transfer %s not found " (trailing space, interpolated).
func (h *Handler) bankTransferByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:transaction:read") {
		return
	}
	id := chi.URLParam(r, "id")
	bt, err := h.repo.BankTransferByID(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	if bt == nil {
		httpx.WriteError(w, httpx.NotFound(fmt.Sprintf("Bank transfer %s not found ", id)))
		return
	}
	httpx.OK(w, bt)
}

// cloudResourcesAll returns all cloud resources: each joined to its
// project — a bare {data:[...]} with NO paging (unlike the
// other admin lists). Empty under greenfield → {data:[]}; the joined DTO is deferred.
func (h *Handler) cloudResourcesAll(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:cloud_resource:read") {
		return
	}
	pg, ok := paging.FromRequest(w, r)
	if !ok {
		return
	}
	// Newest first (sort _id DESC), join each resource to its project (matched by id, keeping
	// {id,name}), drop resources with no matching project, then reduce. Build the shape:
	// {id,data,createdAt,externalId,info,region,type,serviceId,project:{id,name}}. Paging the
	// window also bounds the per-row project join to one page. Optional ?type= filter (the admin
	// table's resource-type dropdown, applied server-side).
	filter := pgdoc.M{}
	if t := r.URL.Query().Get("type"); t != "" {
		filter["type"] = t
	}
	resources, total, err := h.listRawFilteredSortedMaybePaged(r.Context(), "cloudResource", filter, "_id", -1, pg)
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]pgdoc.M, 0, len(resources))
	for _, cr := range resources {
		projID, _ := cr["projectId"].(string)
		if projID == "" {
			continue // drop resources without a project
		}
		proj, err := h.repo.FindByIDRaw(r.Context(), "project", projID)
		if httpx.WriteError(w, err) {
			return
		}
		if proj == nil {
			continue // drop the unmatched
		}
		sd, _ := shapeDeep(cr).(pgdoc.M)
		keep := pgdoc.M{}
		for _, k := range []string{"id", "data", "createdAt", "externalId", "info", "region", "type", "serviceId"} {
			if v, ok := sd[k]; ok {
				keep[k] = v
			}
		}
		keep["project"] = pgdoc.M{"id": idToString(proj["_id"]), "name": proj["name"]}
		out = append(out, keep)
	}
	// total counts every cloudResource row; the rare project-less/unmatched rows dropped above make
	// the last page slightly short of `total` — acceptable for the admin table.
	if pg.Active {
		httpx.Page(w, out, paging.OffsetPaging(pg, total))
		return
	}
	httpx.OK(w, out)
}

// cloudResourceByID returns a cloud resource by id (ADMIN_CLOUD_RESOURCE_READ):
// a missing id yields an empty {} envelope, NOT a 404.
func (h *Handler) cloudResourceByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:cloud_resource:read") {
		return
	}
	res, err := h.cloud.FindByID(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if res == nil {
		httpx.Empty(w)
		return
	}
	httpx.OK(w, res)
}

// cloudResourcesByUser lists a user's cloud resources.
func (h *Handler) cloudResourcesByUser(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:cloud_resource:read") {
		return
	}
	res, err := h.cloud.FindAllByUserID(r.Context(), chi.URLParam(r, "userId"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, res)
}

// cloudResourcesByProject lists a project's cloud resources.
func (h *Handler) cloudResourcesByProject(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:cloud_resource:read") {
		return
	}
	res, err := h.cloud.FindAllByProjectID(r.Context(), chi.URLParam(r, "projectId"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, res)
}

// suspensionsByBP lists the suspension processes for a billing profile
// (ADMIN_SUSPENSION_READ; empty under greenfield → {data:[],paging}).
func (h *Handler) suspensionsByBP(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:suspension:read") {
		return
	}
	procs, err := h.billing.AllSuspensionsByBillingProfile(r.Context(), chi.URLParam(r, "billingProfileId"))
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, procs)
}

// flavorCategories returns the admin flavor-category list
// (gated ADMIN_FLAVOR_CATEGORY_MANAGE). Empty under the greenfield seed.
func (h *Handler) flavorCategories(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:flavor_category:manage") {
		return
	}
	items, err := h.catalog.AllFlavorCategories(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}
