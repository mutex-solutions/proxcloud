package mcp

// clientTools is the toolset for clients-realm principals (a portal sign-in or a client personal
// access token). Every row maps to an existing /api/v1 endpoint; dispatch re-enters the router with
// the caller's own credential, so project/org policy, suspension and quota apply unchanged.
//
// Note: some rows embed a constant query string (e.g. ?type=SERVER) directly
// in the path. dispatch only appends "?"+encoded when the tool declares query
// params, so this is safe as long as such rows declare NO query params.
var clientTools = concatTools(clientCoreTools, clientComputeTools, clientManagedTools, clientSandboxTools)

func concatTools(sets ...[]toolDef) []toolDef {
	out := []toolDef{}
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

// projectID is the path param every project-scoped client tool takes.
var projectID = param{name: "id", typ: "string", desc: "Project id.", required: true, in: "path"}

// resourceID is the cloud resource a per-resource action targets.
func resourceID(desc string) param {
	return param{name: "resourceId", typ: "string", desc: desc, required: true, in: "path"}
}

// listOf is a cached-resource list of one type.
func listOf(name, desc, typ string) toolDef {
	return toolDef{name: name, desc: desc, method: "POST", path: "/api/v1/project/{id}/resource?type=" + typ, params: []param{projectID}}
}

// action is a per-resource cloud action with its action string fixed by the tool.
func action(name, desc, act, resDesc string, extra ...param) toolDef {
	ps := append([]param{projectID, resourceID(resDesc), {name: "action", in: "body", fixed: act}}, extra...)
	return toolDef{name: name, desc: desc, method: "POST", path: "/api/v1/project/{id}/cloud/{resourceId}/action", params: ps}
}

// destructive marks a tool whose name does not say it destroys (a restore, a replace).
func destructive(d toolDef) toolDef {
	d.destructive = true
	return d
}

// dataParam is the action/create payload.
func dataParam(desc string, required bool) param {
	return param{name: "data", typ: "object", desc: desc, required: required, in: "body"}
}

// serviceIDParam picks the provider a create goes to. Without it the API picks the project's
// Proxmox provider, which cannot create clusters or databases.
func serviceIDParam(kind string) param {
	return param{name: "serviceId", typ: "string", desc: "Id of the project's " + kind + " service (from list_project_services).", required: true, in: "query"}
}

var clientCoreTools = []toolDef{
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
		params: []param{projectID},
	},
	{
		name:   "list_project_services",
		desc:   "List the project's cloud services (one per provider: compute, object storage, Kubernetes, databases, DNS, shares, sandboxes) with their id, kind and the catalog the create forms use: sandboxTemplates and sandboxLimits on a sandbox service, the engines on a database service, versions and flavors on a Kubernetes service. A service id is the serviceId of create_kubernetes_cluster and create_database.",
		method: "GET",
		path:   "/api/v1/project/{id}/service",
		params: []param{projectID},
	},
	{
		name:   "get_cloud_resource",
		desc:   "Get one cloud resource (server, volume, network, ...) by its resource id, live-refreshed.",
		method: "GET",
		path:   "/api/v1/project/{id}/cloud/{resourceId}",
		params: []param{projectID, resourceID("Cloud resource id (the id field from a list_* result).")},
	},
	{
		name:   "get_project_billing",
		desc:   "Get the project's billing-profile summary including financials (balance, due).",
		method: "GET",
		path:   "/api/v1/project/{id}/billing",
		params: []param{projectID},
	},
	{
		name:   "get_project_cost",
		desc:   "Get the project's cost overview: current/last month costs, cost by resource type, top cost generators, balance, due amount and credits.",
		method: "GET",
		path:   "/api/v1/project/{id}/cost-info",
		params: []param{projectID},
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

// Lists read the platform's resource cache (kept current by the sync and by every write).
var clientComputeTools = []toolDef{
	listOf("list_servers", "List the project's servers (virtual machines).", "SERVER"),
	listOf("list_volumes", "List the project's block-storage volumes.", "VOLUME"),
	listOf("list_networks", "List the project's networks. A network's id goes in networkIds of create_server.", "NETWORK"),
	listOf("list_floating_ips", "List the project's floating IPs.", "FLOATING_IP"),
	listOf("list_security_groups", "List the project's security groups.", "SECURITY_GROUP"),
	listOf("list_images", "List the project's own images (server templates saved in the project). The public catalog is list_public_images.", "IMAGE"),
	listOf("list_load_balancers", "List the project's load balancers.", "LOAD_BALANCER"),
	{
		name:   "list_flavors",
		desc:   "List the compute flavors (hardware sizes) available to the project. A flavor's id is the flavorId of create_server.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud/action",
		params: []param{projectID, {name: "action", in: "body", fixed: "LIST_FLAVORS"}},
	},
	{
		name:   "list_public_images",
		desc:   "List the public image catalog (operating-system templates). An image's id is the imageId of create_server.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud/action",
		params: []param{projectID, {name: "action", in: "body", fixed: "PUBLIC_IMAGES"}},
	},
	{
		name:   "create_server",
		desc:   "Create a server (virtual machine). Billed from creation; the project's quota and GPU quota apply. Poll get_cloud_resource until it is ACTIVE.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud",
		params: []param{
			projectID,
			{name: "type", in: "body", fixed: "SERVER"},
			dataParam("Server spec: {\"name\": string (required), \"flavorId\": string (required, from list_flavors), \"imageId\": string (required, from list_public_images, or a project image's external id), \"networkIds\": [string] (required, the first is used: a network id from list_networks), \"keyName\": string (an SSH keypair name), \"securityGroupNames\": [string], \"rootVolume\": {\"sizeGiB\": integer, \"type\": string}, \"userData\": string (cloud-init), \"adminUser\": string and \"adminPass\": string (set together)}.", true),
		},
	},
	{
		name:   "server_action",
		desc:   "Run a power action on a server. action: one of REBOOT (soft), HARDREBOOT, START, STOP. Backups, snapshots, restores and protection have their own tools.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud/{resourceId}/action",
		params: []param{
			projectID,
			resourceID("Server resource id (from list_servers)."),
			// Only the power actions: the same route also takes DELETE_BACKUP, ROLLBACK_SNAPSHOT and the like, which
			// have their own tools with destructiveHint, and this tool is annotated non-destructive.
			{name: "action", typ: "string", desc: "One of REBOOT, HARDREBOOT, START, STOP.", required: true, in: "body", enum: []string{"REBOOT", "HARDREBOOT", "START", "STOP"}},
		},
	},
	{
		name:   "create_volume",
		desc:   "Create a volume (data disk). With data.serverId it is created on that server; without it, a standalone detached volume is created, which a server can attach later.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud",
		params: []param{
			projectID,
			{name: "type", in: "body", fixed: "VOLUME"},
			dataParam("Volume spec: {\"sizeGiB\": integer (required), \"serverId\": string (server resource id; omit for a standalone volume), \"availabilityZone\": string (node of a standalone volume, optional), \"type\": string (storage-profile slug, optional)}.", true),
		},
	},
	{
		name:   "delete_volume",
		desc:   "Delete a cloud resource (e.g. a volume) by its resource id. Irreversible: the id decides what is deleted, so pass a volume id from list_volumes. A volume still attached to a server is refused (409); detach it first.",
		method: "DELETE",
		path:   "/api/v1/project/{id}/cloud/{resourceId}",
		params: []param{projectID, resourceID("Volume resource id (from list_volumes).")},
	},
	// Backups: a server's backups (Proxmox vzdump archives) or a managed database's.
	action("list_backups", "List the backups of a server or a managed database. A server backup's id is what restore_server_backup and delete_server_backup take.", "LIST_BACKUPS",
		"Server or database resource id."),
	action("create_backup", "Back up a server or a managed database now. Needs a provider with backup storage; a database engine must support backups.", "CREATE_BACKUP",
		"Server or database resource id."),
	destructive(action("restore_server_backup", "Restore a server from one of its backups. Replaces the server's current disks with the archive's; anything written since is lost. Refused when the result would exceed the project's quota.", "RESTORE_BACKUP",
		"Server resource id.", dataParam("{\"id\": string} (a backup id from list_backups).", true))),
	action("delete_server_backup", "Delete one backup of a server. Irreversible.", "DELETE_BACKUP", "Server resource id.",
		dataParam("{\"id\": string} (a backup id from list_backups).", true)),
	// Snapshots: point-in-time states of a server's disks, kept on the server.
	action("list_server_snapshots", "List a server's snapshots (name, description, createdAt, latest).", "LIST_SNAPSHOTS", "Server resource id."),
	action("create_server_snapshot", "Take a snapshot of a server.", "CREATE_SNAPSHOT", "Server resource id.",
		dataParam("{\"name\": string (required: a letter, then 2 to 40 letters, digits, dashes, underscores), \"description\": string}.", true)),
	destructive(action("rollback_server_snapshot", "Roll a server back to one of its snapshots. Anything written since the snapshot is lost.", "ROLLBACK_SNAPSHOT",
		"Server resource id.", dataParam("{\"name\": string} (from list_server_snapshots).", true))),
	action("delete_server_snapshot", "Delete one snapshot of a server. Irreversible.", "DELETE_SNAPSHOT", "Server resource id.",
		dataParam("{\"name\": string} (from list_server_snapshots).", true)),
	// Protection: the billed "Protected" add-on (high availability across two nodes).
	action("enable_server_protection", "Turn on the Protected add-on for a server: it fails over to another node when its node dies (billed while on). Returns {result: {available, reason, protection}}.", "ENABLE_PROTECTION", "Server resource id."),
	destructive(action("disable_server_protection", "Turn off the Protected add-on for a server: it no longer fails over to another node. Returns {result: {available, reason, protection}}.", "DISABLE_PROTECTION",
		"Server resource id.")),
}

// Managed services: Kubernetes clusters, databases, DNS zones.
var clientManagedTools = []toolDef{
	listOf("list_kubernetes_clusters", "List the project's managed Kubernetes clusters with their status (refreshed on read).", "KUBERNETES_CLUSTER"),
	{
		name:   "create_kubernetes_cluster",
		desc:   "Create a managed Kubernetes cluster. Versions, flavors and image variants are on the Kubernetes service in list_project_services. Its worker nodes are billed as servers; quota and GPU quota apply.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud",
		params: []param{
			projectID,
			serviceIDParam("Kubernetes"),
			{name: "type", in: "body", fixed: "KUBERNETES_CLUSTER"},
			dataParam("Cluster spec: {\"name\": string (required), \"version\": string, \"ha\": boolean, \"networkId\": string, \"allowedCidrs\": [string], \"nodeGroups\": [{\"name\": string, \"flavorId\": string, \"count\": integer, \"autoscale\": boolean, \"min\": integer, \"max\": integer, \"imageVariant\": string, \"rootVolumeGiB\": integer, \"labels\": {string: string}, \"taints\": [\"key=value:Effect\"]}] (required)}.", true),
		},
	},
	action("get_kubeconfig", "Get a cluster's admin kubeconfig. It is a cluster-admin credential: it lands in this conversation, so handle it like a password.", "GET_KUBECONFIG",
		"Kubernetes cluster resource id."),
	destructive(action("set_kubernetes_node_groups", "Replace a cluster's node groups (add, remove, resize). The list is the whole new set: a group left out is deleted with its nodes.", "SET_NODE_GROUPS",
		"Kubernetes cluster resource id.", dataParam("{\"nodeGroups\": [ ...same shape as create_kubernetes_cluster... ]}.", true))),
	action("upgrade_kubernetes_cluster", "Upgrade a cluster to the next Kubernetes minor version (one minor at a time, never backwards). The control plane rolls first, then the node groups.", "UPGRADE",
		"Kubernetes cluster resource id.", dataParam("{\"version\": string} (offered by the Kubernetes service).", true)),
	listOf("list_databases", "List the project's managed databases with their status (refreshed on read).", "DATABASE_CLUSTER"),
	{
		name:   "create_database",
		desc:   "Create a managed database. Engines, versions, replica choices and storage classes are on the database service in list_project_services. Use list_backups and create_backup on it afterwards.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud",
		params: []param{
			projectID,
			serviceIDParam("database"),
			{name: "type", in: "body", fixed: "DATABASE_CLUSTER"},
			dataParam("Database spec: {\"name\": string (required), \"engine\": string (required), \"version\": string, \"replicas\": integer, \"cpu\": integer, \"memoryGiB\": integer, \"storageGiB\": integer, \"storageClass\": string, \"allowedCidrs\": [string]}.", true),
		},
	},
	listOf("list_dns_zones", "List the project's DNS zones.", "DNS_ZONE"),
	action("list_dns_records", "List a DNS zone's records.", "LIST_RECORDS", "DNS zone resource id."),
	destructive(action("set_dns_record", "Create or replace the records of one name and type in a DNS zone (the old records of that name and type are dropped).", "SET_RECORD", "DNS zone resource id.",
		dataParam("{\"name\": string (required), \"type\": string (required, e.g. A, AAAA, CNAME, MX, TXT), \"ttl\": integer, \"records\": [string] (required)}.", true))),
	action("delete_dns_record", "Delete the records of one name and type from a DNS zone.", "DELETE_RECORD", "DNS zone resource id.",
		dataParam("{\"name\": string, \"type\": string} (both required).", true)),
}

// Sandboxes (gVisor sandboxes for running code; docs/managed-sandbox.md §3).
var clientSandboxTools = []toolDef{
	listOf("list_sandboxes", "List the project's sandboxes with their live status (PENDING or RUNNING), template, size, GPU and shutdownTime.", "SANDBOX"),
	{
		name:   "list_sandbox_templates",
		desc:   "List the project's own sandbox templates (built from a Dockerfile) with their build status. The provider's templates are sandboxTemplates in list_project_services.",
		method: "GET",
		path:   "/api/v1/project/{id}/sandbox-templates",
		params: []param{projectID},
	},
	{
		name:   "create_sandbox",
		desc:   "Start an isolated sandbox (gVisor) to run code in. It is RUNNING within seconds; poll list_sandboxes until it is. At its timeout it is deleted, or paused when autoPause is set. Billed per minute while running. The templates (sandboxTemplates) and limits (sandboxLimits) are on the sandbox service in list_project_services; the project's own templates are in list_sandbox_templates.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud",
		params: []param{
			projectID,
			{name: "type", in: "body", fixed: "SANDBOX"},
			dataParam("Sandbox spec: {\"template\": string (required), \"name\": string, \"cpu\": integer, \"memoryGiB\": integer, \"gpu\": {\"model\": string, \"count\": integer}, \"timeoutSeconds\": integer (1 to maxTimeoutSeconds, default defaultTimeoutSeconds), \"env\": {string: string}, \"metadata\": {string: string} (not secret), \"network\": {\"allowOut\": [cidr], \"denyOut\": [cidr]}, \"allowPublicTraffic\": boolean, \"autoPause\": boolean (pause instead of delete at the timeout; 400 where the location cannot pause), \"autoResume\": boolean (needs autoPause, traffic resumes it; 400 where the location has no wake token)}.", true),
		},
	},
	action("exec_in_sandbox", "Run one command in a RUNNING sandbox and wait for it (up to timeoutSeconds, default 60, max 600). Returns {result: {stdout, stderr (each capped at 1 MiB), exitCode, truncated}}. Pass data.cmd for a shell string (run with sh -c) or data.command for an argv array; optional data.stdin.", "EXEC",
		"Sandbox resource id (the id field from list_sandboxes or create_sandbox).",
		dataParam("{\"cmd\": string} or {\"command\": [string]}, plus optional \"stdin\": string and \"timeoutSeconds\": integer.", true)),
	action("get_sandbox_logs", "The newest lines of a sandbox's output (its start command and what it wrote to stdout/stderr).", "GET_LOGS", "Sandbox resource id.",
		dataParam("{\"tailLines\": integer} (default 200, at most 2000).", false)),
	action("get_sandbox_access", "How to reach a sandbox's ports: {result: {domain, host, trafficAccessToken?, envdAccessToken?}}. The tokens are credentials for this sandbox: they land in this conversation.", "GET_ACCESS",
		"Sandbox resource id."),
	action("set_sandbox_timeout", "Keep a sandbox alive longer (or end it sooner): its shutdown moves to now + timeoutSeconds.", "SET_TIMEOUT", "Sandbox resource id.",
		dataParam("{\"timeoutSeconds\": integer}.", true)),
	{
		name:   "delete_sandbox",
		desc:   "Delete a sandbox now (its files are lost). Billing stops at once.",
		method: "DELETE",
		path:   "/api/v1/project/{id}/cloud/{resourceId}",
		params: []param{projectID, resourceID("Sandbox resource id (from list_sandboxes).")},
	},
}
