package mcp

// clientTools is the toolset for clients-realm principals. Every row maps to
// an existing /api/v1 endpoint; dispatch re-enters the router with the
// caller's own bearer, so project/org policy applies unchanged.
//
// Note: some rows embed a constant query string (e.g. ?type=SERVER) directly
// in the path. dispatch only appends "?"+encoded when the tool declares query
// params, so this is safe as long as such rows declare NO query params.
var clientTools = []toolDef{
	{
		name:   "list_projects",
		desc:   "List the projects the authenticated user belongs to.",
		method: "GET",
		path:   "/api/v1/project",
	},
	{
		name:   "get_current_user",
		desc:   "Get the authenticated user's account details (name, email).",
		method: "GET",
		path:   "/api/v1/account/details",
	},
	{
		name:   "get_project",
		desc:   "Get one project by its id (the user must be a member).",
		method: "GET",
		path:   "/api/v1/project/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_servers",
		desc:   "List the project's servers (virtual machines).",
		method: "POST",
		path:   "/api/v1/project/{id}/resource?type=SERVER",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_volumes",
		desc:   "List the project's block-storage volumes.",
		method: "POST",
		path:   "/api/v1/project/{id}/resource?type=VOLUME",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_networks",
		desc:   "List the project's networks.",
		method: "POST",
		path:   "/api/v1/project/{id}/resource?type=NETWORK",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_floating_ips",
		desc:   "List the project's floating IPs (live from the cloud).",
		method: "POST",
		path:   "/api/v1/project/{id}/resource?type=FLOATING_IP",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_security_groups",
		desc:   "List the project's security groups (live from the cloud).",
		method: "POST",
		path:   "/api/v1/project/{id}/resource?type=SECURITY_GROUP",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_images",
		desc:   "List the project's images/snapshots (live from the cloud).",
		method: "POST",
		path:   "/api/v1/project/{id}/resource?type=IMAGE",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_load_balancers",
		desc:   "List the project's load balancers.",
		method: "POST",
		path:   "/api/v1/project/{id}/resource?type=LOAD_BALANCER",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "get_cloud_resource",
		desc:   "Get one cloud resource (server, volume, network, ...) by its resource id, live-refreshed.",
		method: "GET",
		path:   "/api/v1/project/{id}/cloud/{resourceId}",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "resourceId", typ: "string", desc: "Cloud resource id (the id field from a list_* result).", required: true, in: "path"},
		},
	},
	{
		name:   "list_flavors",
		desc:   "List the live compute flavors (hardware sizes) available to the project. action must be the string LIST_FLAVORS.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud/action",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "action", typ: "string", desc: "Always pass \"LIST_FLAVORS\".", required: true, in: "body"},
		},
	},
	{
		name:   "get_project_billing",
		desc:   "Get the project's billing-profile summary including financials (balance, due).",
		method: "GET",
		path:   "/api/v1/project/{id}/billing",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "get_project_cost",
		desc:   "Get the project's cost overview: current/last month costs, cost by resource type, top cost generators, balance, due amount and credits.",
		method: "GET",
		path:   "/api/v1/project/{id}/cost-info",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_billing_profiles",
		desc:   "List the billing profiles the user can access (source of billingProfileId for bill queries).",
		method: "GET",
		path:   "/api/v1/billing-profile",
	},
	{
		name:   "list_bills",
		desc:   "List the bills of a billing profile.",
		method: "GET",
		path:   "/api/v1/bill/{billingProfileId}",
		params: []param{
			{name: "billingProfileId", typ: "string", desc: "Billing profile id.", required: true, in: "path"},
		},
	},
	{
		name:   "list_organizations",
		desc:   "List the organizations the authenticated user belongs to.",
		method: "GET",
		path:   "/api/v1/organizations",
	},
	{
		name:   "list_org_members",
		desc:   "List the members of an organization (name, email, role).",
		method: "GET",
		path:   "/api/v1/organizations/{id}/members",
		params: []param{
			{name: "id", typ: "string", desc: "Organization id.", required: true, in: "path"},
		},
	},
	{
		name:   "server_action",
		desc:   "Run a power action on a server. action: one of REBOOT (soft), HARDREBOOT, START, STOP.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud/{resourceId}/action",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "resourceId", typ: "string", desc: "Server resource id (from list_servers).", required: true, in: "path"},
			{name: "action", typ: "string", desc: "One of REBOOT, HARDREBOOT, START, STOP.", required: true, in: "body"},
		},
	},
	{
		name:   "create_volume",
		desc:   "Create a block-storage volume in the project. type must be the string VOLUME; data carries the volume spec.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "type", typ: "string", desc: "Always pass \"VOLUME\".", required: true, in: "body"},
			{name: "data", typ: "object", desc: "Volume spec: {\"name\": string, \"size\": integer (GB), optional \"type\" (volume type), \"availabilityZone\", \"imageId\"}.", required: true, in: "body"},
		},
	},
	{
		name:   "delete_volume",
		desc:   "Delete a cloud resource (e.g. a volume) by its resource id. Irreversible — the id decides what is deleted, so pass a volume id from list_volumes.",
		method: "DELETE",
		path:   "/api/v1/project/{id}/cloud/{resourceId}",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "resourceId", typ: "string", desc: "Volume resource id (from list_volumes).", required: true, in: "path"},
		},
	},
	{
		name:   "invite_user_to_project",
		desc:   "Invite a user by email to join a project (sends an invite mail, 24h token).",
		method: "POST",
		path:   "/api/v1/project-invites/invite",
		params: []param{
			{name: "email", typ: "string", desc: "Email address to invite.", required: true, in: "body"},
			{name: "projectId", typ: "string", desc: "Project id to invite the user into.", required: true, in: "body"},
		},
	},
}
