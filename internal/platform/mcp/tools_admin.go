package mcp

// adminTools is the toolset for admin principals (master-realm JWT or a
// `Bearer pk.sk` hmac api key). Rows map to /admin-api/v1 endpoints; api-key
// dispatch is SigV4-signed internally, so the Admin API gate applies unchanged.
var adminTools = []toolDef{
	// ── users ────────────────────────────────────────────────────────────────
	{
		name:   "list_users",
		desc:   "List platform users (keyset paginated).",
		method: "GET",
		path:   "/admin-api/v1/users",
		params: []param{
			{name: "marker", typ: "string", desc: "Pagination marker from a previous page's next_marker.", in: "query"},
			{name: "limit", typ: "integer", desc: "Page size (1-500, default 50).", in: "query"},
			{name: "email", typ: "string", desc: "Filter by exact email.", in: "query"},
			{name: "sub", typ: "string", desc: "Filter by subject (matches the user's sub or any identity sub).", in: "query"},
		},
	},
	{
		name:   "get_user",
		desc:   "Get a single user by id.",
		method: "GET",
		path:   "/admin-api/v1/users/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "User id.", required: true, in: "path"},
		},
	},
	{
		name:   "create_user",
		desc:   "Pre-create a user before first login; sub defaults to a generated 'user-<md5>' with issuer 'api'. An existing email returns 409.",
		method: "POST",
		path:   "/admin-api/v1/users",
		params: []param{
			{name: "email", typ: "string", desc: "User email (must not already exist).", required: true, in: "body"},
			{name: "sub", typ: "string", desc: "Optional explicit subject; generated when omitted.", in: "body"},
		},
	},
	{
		name:   "delete_user",
		desc:   "Delete a user by id (200 with empty body on success).",
		method: "DELETE",
		path:   "/admin-api/v1/users/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "User id.", required: true, in: "path"},
		},
	},

	// ── organizations ────────────────────────────────────────────────────────
	{
		name:   "list_organizations",
		desc:   "List organizations (keyset paginated), each with its resolved member list.",
		method: "GET",
		path:   "/admin-api/v1/organizations",
		params: []param{
			{name: "marker", typ: "string", desc: "Pagination marker from a previous page's next_marker.", in: "query"},
			{name: "limit", typ: "integer", desc: "Page size (1-500, default 50).", in: "query"},
			{name: "name", typ: "string", desc: "Filter by exact organization name.", in: "query"},
			{name: "member_sub", typ: "string", desc: "Filter to organizations the given subject is a member of.", in: "query"},
			{name: "billing_profile_id", typ: "string", desc: "Filter by attached billing profile id.", in: "query"},
		},
	},
	{
		name:   "get_organization",
		desc:   "Get a single organization by id, including members.",
		method: "GET",
		path:   "/admin-api/v1/organizations/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Organization id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_org_members",
		desc:   "List an organization's members (returns a bare JSON array, no data envelope).",
		method: "GET",
		path:   "/admin-api/v1/organizations/{id}/members",
		params: []param{
			{name: "id", typ: "string", desc: "Organization id.", required: true, in: "path"},
		},
	},
	{
		name:   "create_organization",
		desc:   "Create an organization; owner_sub (when given) must resolve to an existing user and is added as OWNER.",
		method: "POST",
		path:   "/admin-api/v1/organizations",
		params: []param{
			{name: "name", typ: "string", desc: "Organization name.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Optional description.", in: "body"},
			{name: "owner_sub", typ: "string", desc: "Subject of an existing user to add as OWNER (404 when unknown).", in: "body"},
			{name: "billing_profile_id", typ: "string", desc: "Optional billing profile to attach.", in: "body"},
		},
	},

	{
		name:   "create_organization_billing_profile",
		desc:   "Create the billing profile for an EXISTING organization that has none (e.g. an admin-created org under an operator-only self-service lock). Built from the org's OWNER member; idempotent (no-op if one already exists). Returns the organization with its billingProfileId.",
		method: "POST",
		path:   "/api/v1/admin/organizations/{id}/billing-profile",
		params: []param{
			{name: "id", typ: "string", desc: "Organization id.", required: true, in: "path"},
		},
	},

	// ── projects ─────────────────────────────────────────────────────────────
	{
		name:   "list_projects",
		desc:   "List projects (keyset paginated) with provisioned services and members.",
		method: "GET",
		path:   "/admin-api/v1/projects",
		params: []param{
			{name: "marker", typ: "string", desc: "Pagination marker from a previous page's next_marker.", in: "query"},
			{name: "limit", typ: "integer", desc: "Page size (1-500, default 50).", in: "query"},
			{name: "organization_id", typ: "string", desc: "Filter by owning organization id.", in: "query"},
			{name: "billing_profile_id", typ: "string", desc: "Filter by the project's own billing profile id.", in: "query"},
			{name: "member_sub", typ: "string", desc: "Filter to projects the given subject is a member of.", in: "query"},
			{name: "status", typ: "string", desc: "Filter by project status (e.g. ENABLED, DISABLED).", in: "query"},
			{name: "openstack_project_id", typ: "string", desc: "Filter by the provisioned OpenStack tenant id.", in: "query"},
		},
	},
	{
		name:   "get_project",
		desc:   "Get a single project by id (billing_profile_id is the effective one: the project's own, else the owning org's).",
		method: "GET",
		path:   "/admin-api/v1/projects/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "create_project",
		desc:   "Create a project (status DISABLED, no members); billing_profile_id must exist when given. A non-empty provision array also provisions it onto the platform cloud right after the save.",
		method: "POST",
		path:   "/admin-api/v1/projects",
		params: []param{
			{name: "name", typ: "string", desc: "Project name.", required: true, in: "body"},
			{name: "organization_id", typ: "string", desc: "Owning organization id.", required: true, in: "body"},
			{name: "billing_profile_id", typ: "string", desc: "Optional billing profile id (404 when unknown).", in: "body"},
			// ponytail: handler only checks len(provision)>0 — entry contents are ignored.
			{name: "provision", typ: "array", desc: "Optional array of provision spec objects; any non-empty array triggers provisioning (keystone tenant + ENABLED). Pass e.g. [{}].", in: "body"},
		},
	},
	{
		name:   "provision_project",
		desc:   "Provision an existing project onto the platform cloud (keystone tenant, status ENABLED); returns the refreshed project.",
		method: "POST",
		path:   "/admin-api/v1/projects/{id}/provision",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},

	// ── billing profiles ─────────────────────────────────────────────────────
	{
		name:   "list_billing_profiles",
		desc:   "List billing profiles (keyset paginated).",
		method: "GET",
		path:   "/admin-api/v1/billing_profiles",
		params: []param{
			{name: "marker", typ: "string", desc: "Pagination marker from a previous page's next_marker.", in: "query"},
			{name: "limit", typ: "integer", desc: "Page size (1-500, default 50).", in: "query"},
			{name: "organization_id", typ: "string", desc: "Filter by organization id.", in: "query"},
			{name: "email", typ: "string", desc: "Filter by exact profile email.", in: "query"},
			{name: "member_sub", typ: "string", desc: "Filter by the profile owner's subject.", in: "query"},
		},
	},
	{
		name:   "get_billing_profile",
		desc:   "Get a single billing profile by id.",
		method: "GET",
		path:   "/admin-api/v1/billing_profiles/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Billing profile id.", required: true, in: "path"},
		},
	},
	{
		name:   "activate_billing_profile",
		desc:   "Activate a billing profile (KYC/activation orchestration); a no-op for profiles not in NEW status.",
		method: "POST",
		path:   "/admin-api/v1/billing_profiles/{id}/activate",
		params: []param{
			{name: "id", typ: "string", desc: "Billing profile id.", required: true, in: "path"},
		},
	},
	{
		name:   "suspend_billing_profile",
		desc:   "Suspend a billing profile (pauses its cloud resources and disables its projects).",
		method: "POST",
		path:   "/admin-api/v1/billing_profiles/{id}/suspend",
		params: []param{
			{name: "id", typ: "string", desc: "Billing profile id.", required: true, in: "path"},
		},
	},
	{
		name:   "resume_billing_profile",
		desc:   "Resume (unsuspend) a billing profile; cloud unpause runs asynchronously.",
		method: "POST",
		path:   "/admin-api/v1/billing_profiles/{id}/resume",
		params: []param{
			{name: "id", typ: "string", desc: "Billing profile id.", required: true, in: "path"},
		},
	},

	// ── bills ────────────────────────────────────────────────────────────────
	{
		name:   "list_bills",
		desc:   "List bills (keyset paginated); items are omitted unless include_items=true.",
		method: "GET",
		path:   "/admin-api/v1/bills",
		params: []param{
			{name: "marker", typ: "string", desc: "Pagination marker from a previous page's next_marker.", in: "query"},
			{name: "limit", typ: "integer", desc: "Page size (1-500, default 50).", in: "query"},
			{name: "billing_profile_id", typ: "string", desc: "Filter by billing profile id.", in: "query"},
			{name: "status", typ: "string", desc: "Filter by bill status.", in: "query"},
			{name: "start_date", typ: "string", desc: "RFC3339 timestamp; bills whose cycle starts at or after this.", in: "query"},
			{name: "end_date", typ: "string", desc: "RFC3339 timestamp; bills whose cycle ends at or before this.", in: "query"},
			{name: "include_items", typ: "boolean", desc: "Include line items (default false → items: []).", in: "query"},
		},
	},
	{
		name:   "get_bill",
		desc:   "Get a single bill by id, including its line items.",
		method: "GET",
		path:   "/admin-api/v1/bills/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Bill id.", required: true, in: "path"},
		},
	},

	// ── account credits ──────────────────────────────────────────────────────
	{
		name:   "list_account_credits",
		desc:   "List account credits for a billing profile (keyset paginated; 404 when the profile doesn't exist).",
		method: "GET",
		path:   "/admin-api/v1/account_credits",
		params: []param{
			{name: "billing_profile_id", typ: "string", desc: "Billing profile id to list credits for.", required: true, in: "query"},
			{name: "marker", typ: "string", desc: "Pagination marker from a previous page's next_marker.", in: "query"},
			{name: "limit", typ: "integer", desc: "Page size (1-500, default 50).", in: "query"},
		},
	},
	{
		name:   "get_account_credit",
		desc:   "Get a single account credit by id.",
		method: "GET",
		path:   "/admin-api/v1/account_credits/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Account credit id.", required: true, in: "path"},
		},
	},
	{
		name:   "create_account_credit",
		desc:   "Create an account credit in the platform base currency; 501 when the profile's currency differs from the base currency (exchange-rate integration not wired).",
		method: "POST",
		path:   "/admin-api/v1/account_credits",
		params: []param{
			{name: "billing_profile_id", typ: "string", desc: "Billing profile id (404 when unknown).", required: true, in: "body"},
			{name: "amount", typ: "string", desc: "Credit amount as a decimal string, e.g. \"10.00\".", required: true, in: "body"},
		},
	},
	{
		name:   "delete_account_credit",
		desc:   "Delete an account credit; returns 202 Accepted with an empty body.",
		method: "DELETE",
		path:   "/admin-api/v1/account_credits/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Account credit id.", required: true, in: "path"},
		},
	},

	// ── service providers ────────────────────────────────────────────────────
	{
		name:   "list_service_providers",
		desc:   "List cloud service providers (keyset paginated; secrets are never exposed).",
		method: "GET",
		path:   "/admin-api/v1/service_providers",
		params: []param{
			{name: "marker", typ: "string", desc: "Pagination marker from a previous page's next_marker.", in: "query"},
			{name: "limit", typ: "integer", desc: "Page size (1-500, default 50).", in: "query"},
		},
	},
	{
		name:   "get_service_provider",
		desc:   "Get a single service provider by id (identity URL + domain id only, no secrets).",
		method: "GET",
		path:   "/admin-api/v1/service_providers/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Service provider id.", required: true, in: "path"},
		},
	},
}
