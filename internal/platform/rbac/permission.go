// Package rbac is the tenant authorization kernel: the Permission keys, the
// wildcard PermissionMatcher, and the static built-in role → permission tables.
// Pure + deterministic (no DB), golden-tested.
package rbac

// Permission keys (resource:action).
// 23 permissions total.
const (
	OrganizationRead          = "organization:read"
	OrganizationCreate        = "organization:create"
	OrganizationUpdate        = "organization:update"
	OrganizationDelete        = "organization:delete"
	OrganizationManageMembers = "organization:manage_members"
	OrganizationManageRoles   = "organization:manage_roles"
	OrganizationAudit         = "organization:audit"

	ProjectCreate              = "project:create"
	ProjectUpdate              = "project:update"
	ProjectDelete              = "project:delete"
	ProjectManageMembers       = "project:manage_members"
	ProjectCloudResourceRead   = "project:cloud_resource:read"
	ProjectCloudResourceManage = "project:cloud_resource:manage"
	ProjectCloudResourceAPIAcc = "project:cloud_resource:api_access"

	BillingProfileRead                 = "billing_profile:read"
	BillingProfileCreate               = "billing_profile:create"
	BillingProfileUpdate               = "billing_profile:update"
	BillingProfileDelete               = "billing_profile:delete"
	BillingProfileReadInvoices         = "billing_profile:read_invoices"
	BillingProfileDownloadInvoices     = "billing_profile:download_invoices"
	BillingProfileManagePaymentMethods = "billing_profile:manage_payment_methods"
	BillingProfileAddFunds             = "billing_profile:add_funds"
	BillingProfileReadTransactions     = "billing_profile:read_transactions"
)

// descriptions for each permission (used in 403 messages).
var descriptions = map[string]string{
	OrganizationRead:                   "View organization details",
	OrganizationCreate:                 "Create new organizations",
	OrganizationUpdate:                 "Edit organization settings/metadata",
	OrganizationDelete:                 "Delete organizations",
	OrganizationManageMembers:          "Add/remove users from organization",
	OrganizationManageRoles:            "Assign roles to organization members",
	OrganizationAudit:                  "View audit logs",
	ProjectCreate:                      "Create new projects within an organization",
	ProjectUpdate:                      "Edit project settings",
	ProjectDelete:                      "Delete projects",
	ProjectManageMembers:               "Add/remove project members",
	ProjectCloudResourceRead:           "View cloud resources without API credential access",
	ProjectCloudResourceManage:         "Read and write cloud resources via UI without API credential access",
	ProjectCloudResourceAPIAcc:         "Full cloud resource access including API credential management",
	BillingProfileRead:                 "View billing profile details",
	BillingProfileCreate:               "Create new billing profiles",
	BillingProfileUpdate:               "Edit payment methods, billing info",
	BillingProfileDelete:               "Delete billing profiles",
	BillingProfileReadInvoices:         "View invoices",
	BillingProfileDownloadInvoices:     "Download invoice PDFs",
	BillingProfileManagePaymentMethods: "Add/remove payment methods",
	BillingProfileAddFunds:             "Add funds to billing profile",
	BillingProfileReadTransactions:     "View payment history",
}

// Description returns a permission's human description (for 403 messages).
func Description(key string) string { return descriptions[key] }

// AllPermissions is the full key set, used by ExpandPatterns.
var AllPermissions = []string{
	OrganizationRead, OrganizationCreate, OrganizationUpdate, OrganizationDelete,
	OrganizationManageMembers, OrganizationManageRoles, OrganizationAudit,
	ProjectCreate, ProjectUpdate, ProjectDelete, ProjectManageMembers,
	ProjectCloudResourceRead, ProjectCloudResourceManage, ProjectCloudResourceAPIAcc,
	BillingProfileRead, BillingProfileCreate, BillingProfileUpdate, BillingProfileDelete,
	BillingProfileReadInvoices, BillingProfileDownloadInvoices,
	BillingProfileManagePaymentMethods, BillingProfileAddFunds, BillingProfileReadTransactions,
}
