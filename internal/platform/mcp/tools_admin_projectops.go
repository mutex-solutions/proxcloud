package mcp

// adminProjectOpsTools drive per-project operator actions through the internal
// /api/v1/admin routes (quota, public networks, sync, status, reassignment).
var adminProjectOpsTools = []toolDef{
	{
		name:   "set_project_quota",
		desc:   "Replace a project's quota document, enforced when resources are created or resized through ProxCloud. Keys (non-negative integers): servers, vcpus, ramMb, diskGb, networks, floatingIps; sandboxes, sandboxVcpus, sandboxRamMb, sandboxGpus; and gpu = {\"<model>\"|\"*\": n} (per-GPU-model device limits, \"*\" = any model). Once the document sets any non-sandbox key, an absent non-sandbox key is unlimited; without one, the provider's default quota applies to them. An absent sandbox key falls back to the sandbox provider's default. An empty object {} clears the project's quota, so the provider's default quota applies again (unlimited only when the provider has none).",
		method: "PUT",
		path:   "/api/v1/admin/project/{id}/quota",
		params: []param{
			{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"},
			{name: "quota", typ: "object", desc: "The quota document, e.g. {\"servers\": 10, \"vcpus\": 20, \"gpu\": {\"nvidia-a6000\": 4}}.", required: true, in: "rawbody"},
		},
	},
	{
		name:   "set_project_public_networks",
		desc:   "Set a project's public-network allow-list. null = all of the provider's public networks allowed (default); [] = none; [ids] = only those public network ids.",
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
