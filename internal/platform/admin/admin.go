// Package admin is the admin-API authorization kernel + the /api/v1/admin/** surface.
// Admin auth reuses the SAME OIDC token as the client API (no separate realm): a request is
// "admin" iff the authenticated sub has an `adminPermission` document. The doc's role resolves
// to a granted permission-pattern set (built-in roles below, or a custom adminRole — deferred);
// endpoints gate on an admin-permission key via the shared wildcard matcher (`admin:*` etc.).
package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/paging"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// AdminPermission — the adminPermission collection; looked up by sub, where _id == sub.
type AdminPermission struct {
	Sub     string `json:"id,omitempty"`
	Email   string `json:"email,omitempty"`
	Role    string `json:"role,omitempty"`
	Pending bool   `json:"pending"`
}

type Repo struct {
	col *pgdoc.Store
	db  *pgdoc.DB
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{col: db.C("adminPermission"), db: db}
}

// ListRaw returns a collection's documents as raw maps (never nil) — the empty-state admin
// list reads (populated DTO shaping deferred; a populated collection fails loud).
func (r *Repo) ListRaw(ctx context.Context, collection string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c(collection).Find(ctx, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListRawPage is the offset-paged variant of ListRaw (newest-first by _id) → page + total.
func (r *Repo) ListRawPage(ctx context.Context, collection string, p paging.Params) ([]pgdoc.M, int64, error) {
	return paging.Offset[pgdoc.M](ctx, r.c(collection), pgdoc.M{}, nil, p)
}

// FindBySub returns the adminPermission whose id equals the given sub.
func (r *Repo) FindBySub(ctx context.Context, sub string) (*AdminPermission, error) {
	if sub == "" {
		return nil, nil
	}
	var ap AdminPermission
	found, err := r.col.Get(ctx, sub, &ap)
	if err != nil || !found {
		return nil, err
	}
	return &ap, nil
}

// rolePermissions returns the granted patterns for a built-in role. A custom role
// (not built-in) resolves via the adminRole collection — deferred → empty (no admin access).
func rolePermissions(role string) []string {
	switch role {
	case "SUPER_ADMIN":
		return []string{"admin:*"}
	case "ADMIN":
		return []string{
			"admin:user:*", "admin:organization:*", "admin:project:*", "admin:cloud_resource:*",
			"admin:billing_config:*", "admin:billing_profile:*", "admin:bill:*", "admin:transaction:*",
			"admin:price_plan:*", "admin:account_credit:*", "admin:promotional_credit:*", "admin:suspension:*",
			"admin:savings_plan:*", "admin:tax:*", "admin:platform_config:read", "admin:menu:*",
			"admin:message_template:*", "admin:service:*", "admin:integration:*", "admin:flavor_category:*",
			"admin:image_group:*", "admin:audit:read", "admin:role:read", "admin:permission:read",
			"admin:hmac_key:*", "admin:stats:read",
		}
	case "SUPPORT":
		return []string{
			"admin:user:read", "admin:user:manage_credentials", "admin:organization:read", "admin:project:read",
			"admin:cloud_resource:read", "admin:billing_config:read", "admin:billing_profile:read", "admin:bill:read",
			"admin:transaction:read", "admin:price_plan:read", "admin:account_credit:read", "admin:suspension:read",
			"admin:savings_plan:read", "admin:tax:read", "admin:platform_config:read", "admin:message_template:read",
			"admin:service:read", "admin:integration:read", "admin:audit:read", "admin:role:read",
			"admin:permission:read", "admin:stats:read",
		}
	case "BILLING_ADMIN":
		return []string{
			"admin:user:read", "admin:organization:read", "admin:billing_config:*", "admin:billing_profile:*",
			"admin:bill:*", "admin:transaction:*", "admin:price_plan:*", "admin:account_credit:*",
			"admin:promotional_credit:*", "admin:suspension:*", "admin:savings_plan:*", "admin:tax:*", "admin:stats:read",
		}
	case "VIEWER":
		return []string{
			"admin:user:read", "admin:organization:read", "admin:project:read", "admin:cloud_resource:read",
			"admin:billing_config:read", "admin:billing_profile:read", "admin:bill:read", "admin:transaction:read",
			"admin:price_plan:read", "admin:account_credit:read", "admin:suspension:read", "admin:savings_plan:read",
			"admin:tax:read", "admin:platform_config:read", "admin:message_template:read", "admin:service:read",
			"admin:integration:read", "admin:audit:read", "admin:role:read", "admin:permission:read",
			"admin:stats:read",
		}
	default:
		return nil
	}
}

// Permissions resolves the granted pattern set for a sub (empty when the sub has no
// adminPermission). A doc with a null role defaults to SUPER_ADMIN.
func (r *Repo) Permissions(ctx context.Context, sub string) ([]string, error) {
	ap, err := r.FindBySub(ctx, sub)
	if err != nil || ap == nil {
		return nil, err
	}
	role := ap.Role
	if role == "" {
		role = "SUPER_ADMIN"
	}
	return rolePermissions(role), nil
}

// AllPermissionKeys is the full admin-permission key set; the
// /admin/me expanded-permission list comes from filtering this by the granted patterns.
var AllPermissionKeys = []string{
	"admin:account_credit:manage", "admin:account_credit:read", "admin:audit:read",
	"admin:bill:manage", "admin:bill:read", "admin:billing_config:read", "admin:billing_config:update",
	"admin:billing_profile:read", "admin:billing_profile:update", "admin:cloud_resource:manage",
	"admin:cloud_resource:read", "admin:flavor_category:manage", "admin:hmac_key:manage",
	"admin:image_group:manage", "admin:instance_metadata:manage", "admin:integration:manage",
	"admin:integration:read", "admin:menu:manage",
	"admin:message_template:manage", "admin:message_template:read", "admin:organization:delete",
	"admin:organization:manage_roles", "admin:organization:read", "admin:organization:update",
	"admin:permission:manage", "admin:permission:read", "admin:platform_config:read",
	"admin:platform_config:update", "admin:price_plan:manage", "admin:price_plan:read",
	"admin:project:create", "admin:project:delete", "admin:project:import", "admin:project:manage",
	"admin:project:read", "admin:project:update", "admin:promotional_credit:manage",
	"admin:role:manage", "admin:role:read", "admin:savings_plan:manage", "admin:savings_plan:read",
	"admin:service:manage", "admin:service:read", "admin:stats:read", "admin:suspension:manage",
	"admin:suspension:read", "admin:tax:manage", "admin:tax:read", "admin:transaction:manage",
	"admin:transaction:read", "admin:user:create", "admin:user:delete", "admin:user:impersonate",
	"admin:user:manage_credentials", "admin:user:read", "admin:user:update",
}

// PermissionMeta is one {key, description} entry of the admin-permission metadata
// (the available-permissions list → a list of {key, description} maps).
type PermissionMeta struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// AdminPermissionMeta is the full admin-permission metadata in declaration order, each mapped to
// {key, description}. Order is irrelevant for comparison (the harness sorts arrays) but kept in the
// original declaration order.
var AdminPermissionMeta = []PermissionMeta{
	{"admin:user:read", "View user details"},
	{"admin:user:create", "Create users"},
	{"admin:user:update", "Edit user details"},
	{"admin:user:delete", "Delete users"},
	{"admin:user:impersonate", "Impersonate users"},
	{"admin:user:manage_credentials", "Manage user credentials"},
	{"admin:organization:read", "View organizations"},
	{"admin:organization:update", "Edit organizations"},
	{"admin:organization:delete", "Delete organizations"},
	{"admin:organization:manage_roles", "Manage organization roles"},
	{"admin:project:read", "View projects"},
	{"admin:project:create", "Create projects"},
	{"admin:project:update", "Edit projects"},
	{"admin:project:delete", "Delete projects"},
	{"admin:project:import", "Import projects"},
	{"admin:project:manage", "Manage project members"},
	{"admin:cloud_resource:read", "View cloud resources"},
	{"admin:cloud_resource:manage", "Manage cloud resources"},
	{"admin:billing_config:read", "View billing configuration"},
	{"admin:billing_config:update", "Update billing configuration"},
	{"admin:billing_profile:read", "View billing profiles"},
	{"admin:billing_profile:update", "Manage billing profiles"},
	{"admin:bill:read", "View bills"},
	{"admin:bill:manage", "Manage bills"},
	{"admin:transaction:read", "View transactions"},
	{"admin:transaction:manage", "Manage transactions"},
	{"admin:price_plan:read", "View price plans"},
	{"admin:price_plan:manage", "Manage price plans"},
	{"admin:account_credit:read", "View account credits"},
	{"admin:account_credit:manage", "Manage account credits"},
	{"admin:promotional_credit:manage", "Manage promotional credits"},
	{"admin:suspension:read", "View suspensions"},
	{"admin:suspension:manage", "Manage suspensions"},
	{"admin:savings_plan:read", "View savings plans"},
	{"admin:savings_plan:manage", "Manage savings plans"},
	{"admin:tax:read", "View tax rates"},
	{"admin:tax:manage", "Manage tax rates"},
	{"admin:platform_config:read", "View platform configuration"},
	{"admin:platform_config:update", "Update platform configuration"},
	{"admin:menu:manage", "Manage custom menu items"},
	{"admin:message_template:read", "View message templates"},
	{"admin:message_template:manage", "Manage message templates"},
	{"admin:service:read", "View external services"},
	{"admin:service:manage", "Manage external services"},
	{"admin:integration:read", "View third-party integrations"},
	{"admin:integration:manage", "Manage third-party integrations"},
	{"admin:flavor_category:manage", "Manage flavor categories"},
	{"admin:image_group:manage", "Manage image groups"},
	{"admin:instance_metadata:manage", "Manage instance metadata options"},
	{"admin:audit:read", "View audit logs"},
	{"admin:role:read", "View admin roles"},
	{"admin:role:manage", "Manage admin roles"},
	{"admin:permission:read", "View admin permissions"},
	{"admin:permission:manage", "Manage admin permissions"},
	{"admin:hmac_key:manage", "Manage HMAC keys"},
	{"admin:stats:read", "View statistics"},
}

// ListBankTransfers returns the bank transfers for a payment gateway, newest first.
// Raw-domain passthrough (empty under greenfield → []; populated DTO shaping deferred, fails loud).
func (r *Repo) ListBankTransfers(ctx context.Context, integrationId string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c("bankTransfer").Find(ctx, pgdoc.M{"paymentGatewayId": integrationId}, &out,
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime))); err != nil {
		return nil, err
	}
	return out, nil
}

// BankTransferByID looks up a bank transfer by id; nil when
// absent (the caller maps that to the 404 "Bank transfer %s not found ").
func (r *Repo) BankTransferByID(ctx context.Context, id string) (pgdoc.M, error) {
	return r.FindByIDRaw(ctx, "bankTransfer", id)
}

// FindByIDRaw is a generic by-id lookup over any collection → raw pgdoc.M, or (nil,nil) when absent.
// Backs the admin by-id reads. Ids are plain strings (the id column); a malformed/unknown id is
// simply "not found".
func (r *Repo) FindByIDRaw(ctx context.Context, collection, id string) (pgdoc.M, error) {
	return r.FindDoc(ctx, collection, id)
}

// ListExternalResourceProviders finds providers by externalServiceId:
// all providers when externalServiceId is blank, else filtered. Empty under greenfield → [].
func (r *Repo) ListExternalResourceProviders(ctx context.Context, externalServiceID string) ([]pgdoc.M, error) {
	filter := pgdoc.M{}
	if externalServiceID != "" {
		filter["externalServiceId"] = externalServiceID
	}
	out := []pgdoc.M{}
	if err := r.c("externalResourceProvider").Find(ctx, filter, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CountDocs counts a collection (backs the onboarding-status existence checks).
func (r *Repo) CountDocs(ctx context.Context, collection string) (int64, error) {
	return r.c(collection).Count(ctx, nil)
}

// InstalledThirdParties returns the set of `thirdParty` values present in the thirdPartyIntegration
// collection (one existence check per third party). Backs the integrations/stats `installed` flag.
func (r *Repo) InstalledThirdParties(ctx context.Context) (map[string]bool, error) {
	vals, err := r.c("thirdPartyIntegration").Distinct(ctx, "thirdParty", nil)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(vals))
	for _, v := range vals {
		set[v] = true
	}
	return set, nil
}

// ThirdPartyStats is one integrations/stats element.
type ThirdPartyStats struct {
	Name       string   `json:"name"`
	Categories []string `json:"categories"`
	Installed  bool     `json:"installed"`
}

// ThirdPartyCatalog is the static set of integration definitions
// (installed → false under greenfield, since nothing is configured). Order is irrelevant
// (the harness sorts arrays). `installed` is always false here (no integration is configured yet).
var ThirdPartyCatalog = []ThirdPartyStats{
	{"Built-in-Invoicing", []string{"Invoice"}, false},
	{"Stripe", []string{"Invoice", "Payment"}, false},
	{"BankTransfer", []string{"Payment"}, false},
	{"SMTP", []string{"Mail"}, false},
}

// OpenstackServiceTypes is the static set of supported OpenStack service types
// (order irrelevant — the comparison harness sorts arrays).
var OpenstackServiceTypes = []string{
	"compute", "network", "identity", "dns", "orchestration", "image", "load-balancer",
	"container-infra", "object-store", "volumev3", "vstorage", "metric", "sharev2",
	"baremetal", "workloads", "key-manager",
}

// DefaultPlatformBranding reads the default platformConfiguration's branding (name/logo/faviconUrl)
// for the admin /init response. Missing fields stay "" (omitted when empty).
func (r *Repo) DefaultPlatformBranding(ctx context.Context) (id, name, logo, faviconURL string, err error) {
	var doc struct {
		ID       string `json:"id"`
		Branding struct {
			Name       string `json:"name"`
			Logo       string `json:"logo"`
			FaviconURL string `json:"faviconUrl"`
		} `json:"branding"`
	}
	col := r.c("platformConfiguration")
	found, e := col.FindOne(ctx, pgdoc.M{"defaultConfiguration": true}, &doc)
	if e == nil && !found {
		found, e = col.FindOne(ctx, nil, &doc)
	}
	if e != nil || !found {
		return "", "", "", "", e
	}
	return doc.ID, doc.Branding.Name, doc.Branding.Logo, doc.Branding.FaviconURL, nil
}

// ListRawFiltered finds documents matching filter as raw pgdoc.M (never nil) — backs the admin
// by-organization / by-billing-profile / by-user list reads (empty greenfield → []; populated
// typed DTO deferred, fails loud, billing-list precedent).
func (r *Repo) ListRawFiltered(ctx context.Context, collection string, filter pgdoc.M) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c(collection).Find(ctx, normFilter(filter), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// OrganizationsByMemberSub returns the organizations a sub belongs to
// (organization_members join → organization). Empty greenfield → [].
func (r *Repo) OrganizationsByMemberSub(ctx context.Context, sub string) ([]pgdoc.M, error) {
	var members []struct {
		OrganizationID string `json:"organizationId"`
	}
	if err := r.c("organization_members").Find(ctx, pgdoc.M{"sub": sub}, &members); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		if m.OrganizationID != "" {
			ids = append(ids, m.OrganizationID)
		}
	}
	if len(ids) == 0 {
		return []pgdoc.M{}, nil
	}
	return r.ListRawFiltered(ctx, "organization", pgdoc.M{"_id": pgdoc.M{"$in": ids}})
}

// ExpandPatterns returns every admin-permission key
// matched by any granted pattern (admin:* → all 58 keys).
func ExpandPatterns(patterns []string) []string {
	out := make([]string, 0, len(AllPermissionKeys))
	for _, k := range AllPermissionKeys {
		if rbac.Matches(patterns, k) {
			out = append(out, k)
		}
	}
	return out
}

// RequirePermission gates an admin endpoint on an admin-permission key (e.g.
// "admin:flavor_category:manage"): 403 unless the sub's granted patterns match.
func (r *Repo) RequirePermission(ctx context.Context, sub, key string) error {
	perms, err := r.Permissions(ctx, sub)
	if err != nil {
		return err
	}
	if !rbac.Matches(perms, key) {
		return httpx.Forbidden("You do not have the required permission: " + key)
	}
	return nil
}
