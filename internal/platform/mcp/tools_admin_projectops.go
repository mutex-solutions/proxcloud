package mcp

// adminProjectOpsTools drive per-project operator actions through the internal
// /api/v1/admin routes (quota, public networks, sync, status, reassignment).
var adminProjectOpsTools = []toolDef{
	{
		name:   "set_project_quota",
		desc:   "Set a project's quota config. Body is the quota document verbatim, e.g. {\"gpu\": {\"nvidia-a6000\": 4, \"*\": 8}} — per-GPU-model device limits (\"*\" = any model), enforced when servers are created/resized through ProxCloud. An empty object {} clears the quota (unlimited).",
		method: "PUT",
		path:   "/api/v1/admin/project/{id}/quota",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "quota", typ: "object", desc: "The quota document, e.g. {\"gpu\": {\"nvidia-a6000\": 4}}.", required: true, in: "rawbody"},
		},
	},
	{
		name:   "set_project_public_networks",
		desc:   "Set a project's external-network allow-list. null = all router:external networks allowed (default); [] = none; [ids] = only those public network ids.",
		method: "PUT",
		path:   "/api/v1/admin/project/{id}/public-networks",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "publicNetworkIds", typ: "array", desc: "Public network ids to allow; omit for default-all.", in: "body"},
		},
	},
	{
		name:   "sync_project",
		desc:   "Trigger a full cloud sync for one project (reconciles the cached cloud resources against the live cloud).",
		method: "POST",
		path:   "/api/v1/admin/project/{id}/sync",
		params: []param{{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"}},
	},
	{
		name:   "get_project_resource_counts",
		desc:   "Per-type cached cloud-resource counts for a project ({TYPE: n, TOTAL: n}).",
		method: "GET",
		path:   "/api/v1/admin/project/{id}/resources/counts",
		params: []param{{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"}},
	},
	{
		name:   "list_project_cloud_resources",
		desc:   "List a project's cached cloud resources (servers, networks, volumes, ...).",
		method: "GET",
		path:   "/api/v1/admin/cloud-resource/project/{id}",
		params: []param{{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"}},
	},
	{
		name:   "update_project",
		desc:   "Update a project's name / organization / billing profile. WARNING: all three are overwritten together — resend the current values for fields you keep.",
		method: "PUT",
		path:   "/api/v1/admin/project/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Project name.", required: true, in: "body"},
			{name: "organizationId", typ: "string", desc: "Owning organization id.", in: "body"},
			{name: "billingProfileId", typ: "string", desc: "Billing profile id (empty = inherit the organization's).", in: "body"},
		},
	},
	{
		name:   "set_project_status",
		desc:   "Enable or disable a project (DISABLED pauses its servers; ENABLED unpauses).",
		method: "POST",
		path:   "/api/v1/admin/project/{id}/{status}",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "status", typ: "string", desc: "ENABLED or DISABLED.", required: true, in: "path"},
		},
	},
	{
		name:   "list_project_members_admin",
		desc:   "List a project's members (resolved user docs).",
		method: "GET",
		path:   "/api/v1/admin/project/{id}/members",
		params: []param{{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"}},
	},
}
