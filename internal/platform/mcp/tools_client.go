package mcp

// clientTools is the toolset for clients-realm principals (a portal sign-in or a client personal
// access token). Every row maps to an existing /api/v1 endpoint; dispatch re-enters the router with
// the caller's own credential, so project/org policy, suspension and quota apply unchanged.
//
// Note: some rows embed a constant query string (e.g. ?type=SERVER) directly
// in the path. dispatch only appends "?"+encoded when the tool declares query
// params, so this is safe as long as such rows declare NO query params.
var clientTools = concatTools(clientCoreTools, clientComputeTools, clientManagedTools, clientSandboxTools, clientFunctionTools)

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
		desc:   "Get the project's cost overview: current/last month costs, cost by resource type, top cost generators, balance, due amount and credits, plus an estimated month-end cost (forecastedMonthEndCosts; forecastApproximate says when part of it is held at its current cost).",
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
		desc:   "Create a volume (data disk). With data.serverId it is created on that server; without it, a standalone detached volume is created, which a server can attach later. Returns the created volume (its resource id, externalId, and data.volume with name, size, status).",
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
	listOf("list_dns_zones", "List the project's DNS zones (existing legacy zones remain manageable without a new proof).", "DNS_ZONE"),
	{
		name:   "create_dns_ownership_challenge",
		desc:   "Start a nonexclusive DNS ownership challenge before creating a zone. Returns a TXT recordValue ONCE; publish it at the current independent DNS host, not a zone created here first. Pending expires in 30 minutes and reserves nothing. Then verify_dns_ownership and create_dns_zone within 10 minutes. Requires project cloud-manage.",
		method: "POST", path: "/api/v1/project/{id}/dns-ownership",
		params: []param{projectID,
			{name: "serviceId", typ: "string", in: "body", required: true, desc: "Attached PowerDNS provider id."},
			{name: "name", typ: "string", in: "body", required: true, desc: "Domain to prove (IDNA is canonicalized)."}},
	},
	{
		name: "get_dns_ownership", desc: "Get the scoped DNS challenge state and deadlines, never the token. A lost token requires a new challenge.",
		method: "GET", path: "/api/v1/project/{id}/dns-ownership/{ownershipId}",
		params: []param{projectID, {name: "ownershipId", typ: "string", in: "path", required: true, desc: "Challenge id."}, serviceIDParam("DNS")},
	},
	{
		name: "verify_dns_ownership", desc: "Verify the exact TXT value through operator-trusted fixed resolvers before ANY zone is created. CNAME indirection is refused. Wait at least 10 seconds between retries and allow DNS cache propagation. A successful proof is scoped and usable once for 10 minutes; it does not reserve the domain.",
		method: "POST", path: "/api/v1/project/{id}/dns-ownership/{ownershipId}/verify",
		params: []param{projectID, {name: "ownershipId", typ: "string", in: "path", required: true, desc: "Challenge id."},
			{name: "serviceId", typ: "string", in: "body", required: true, desc: "Same attached PowerDNS provider id."}},
	},
	{
		name: "cancel_dns_ownership", desc: "Cancel a DNS ownership challenge. This never deletes a zone or unlocks an existing domain; a pending challenge reserved nothing.",
		method: "DELETE", path: "/api/v1/project/{id}/dns-ownership/{ownershipId}",
		params: []param{projectID, {name: "ownershipId", typ: "string", in: "path", required: true, desc: "Challenge id."}, serviceIDParam("DNS")},
	},
	{
		name: "create_dns_zone", desc: "Create a zone ONLY after verify_dns_ownership succeeded for this project, provider and exact name. The fresh proof is consumed before the upstream request; after an uncertain failure check list_dns_zones, and start a new challenge if absent. Keep current DNS hosting until proof succeeds, then delegate to the shown name servers.",
		method: "POST", path: "/api/v1/project/{id}/cloud",
		params: []param{projectID, serviceIDParam("DNS"), {name: "type", in: "body", fixed: "DNS_ZONE"},
			dataParam("{\"name\": string, \"ownershipId\": string} (both required; id from a fresh verified challenge).", true)},
	},
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
		desc:   "Start an isolated sandbox (gVisor) to run code in. It is RUNNING within seconds; poll list_sandboxes until it is. At its timeout it is deleted. Managed pause/resume/fork and autoPause/autoResume are currently held with a fixed 403; named snapshot capture and ordinary CREATE are separate capabilities. Billed per minute while running. The templates (sandboxTemplates) and limits (sandboxLimits) are on the sandbox service in list_project_services; the project's own templates are in list_sandbox_templates.",
		method: "POST",
		path:   "/api/v1/project/{id}/cloud",
		params: []param{
			projectID,
			{name: "type", in: "body", fixed: "SANDBOX"},
			dataParam("Sandbox spec: {\"template\": string (required), \"name\": string, \"cpu\": integer, \"memoryGiB\": integer, \"gpu\": {\"model\": string, \"count\": integer}, \"timeoutSeconds\": integer (1 to maxTimeoutSeconds, default defaultTimeoutSeconds), \"env\": {string: string}, \"metadata\": {string: string} (not secret), \"network\": {\"allowOut\": [cidr], \"denyOut\": [cidr]}, \"allowPublicTraffic\": boolean, \"autoPause\": boolean (currently held with 403; do not request), \"autoResume\": boolean (currently held with 403; existing invalid combination/configuration checks still return 400)}.", true),
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

// functionID is the function a per-function tool targets.
var functionID = param{name: "fn", typ: "string", desc: "Function id (fn-…), the id field from list_functions or create_function.", required: true, in: "path"}

// The registry credentials of a function, as create, update and rollback take them. A nulls param must come after
// the field it removes: dispatch refuses the two together only when the field was already read.
var (
	registryAuthParam       = param{name: "registryAuth", typ: "object", desc: "Credentials for an image in a private registry: {username, password, server}. server is optional and defaults to the registry of the image (Docker Hub for a bare name). The password is write-only: it is stored sealed and never returned by any call, so it is given whole every time the credentials are set. The platform does not check them: a rollout that cannot pull fails and says so. They land in this conversation.", in: "body"}
	removeRegistryAuthParam = param{name: "removeRegistryAuth", typ: "boolean", desc: "true removes the stored private registry credentials (not together with registryAuth).", in: "body", nulls: "registryAuth"}
)

// clientFunctionTools are the tools of serverless functions (/api/v1/project/{id}/functions): a container image
// behind a stable internal HTTPS URL that starts on a request and stops when idle. No tool takes a GPU, a source to
// build from, a public endpoint or a hostname of its own: the API refuses those, and there is nothing to call.
var clientFunctionTools = []toolDef{
	{
		name:   "list_functions",
		desc:   "List the project's serverless functions with their URL, status (DEPLOYING, READY, IDLE, SUSPENDED or FAILED), live instance count, size and current deployment. Secrets are listed by name only (secretNames), private registry credentials as registryAuth {server, username, setAt} without the password.",
		method: "GET",
		path:   "/api/v1/project/{id}/functions",
		params: []param{projectID},
	},
	{
		name:   "get_function",
		desc:   "Get one function: URL, status, live instance count, config, lastError of a failed deployment, and reason: while DEPLOYING what the rollout is waiting for, and while IDLE with an instance that was asked for and is not ready yet (after a resume, or a request) what that start is waiting for. reason is one of a fixed set of sentences, never the cluster's own text. Secrets are shown by name only (secretNames, and secretsSetAt: when each was last set); their values are never returned. Private registry credentials are shown as registryAuth {server, username, setAt}.",
		method: "GET",
		path:   "/api/v1/project/{id}/functions/{fn}",
		params: []param{projectID, functionID},
	},
	{
		name:   "create_function",
		desc:   "Deploy a container image as a serverless function. It gets a stable internal HTTPS URL (https://<name>-<6 hex>.<the location's sandbox domain>), starts on the first request, scales out to maxInstances on load and to zero when idle, and is billed per minute only while instances run. The container must listen for HTTP on the port (also given to it as PORT). It returns at once with status DEPLOYING; poll get_function until READY, or FAILED with lastError. The URL has no caller authentication and no rate limit: whoever reaches the platform network's proxy address and knows the host name can call the function, each call keeps it running, and its owner pays for that. A GPU, a build from a repository, a public endpoint and a hostname of its own are not offered: a request that names one is refused.",
		method: "POST",
		path:   "/api/v1/project/{id}/functions",
		params: []param{
			projectID,
			{name: "name", typ: "string", desc: "1-40 lowercase letters, digits or '-', starting with a letter and ending with a letter or digit; unique in the project. It is the first part of the URL.", required: true, in: "body"},
			{name: "image", typ: "string", desc: "Container image reference, from any registry with a DNS name the platform's machines reach (no IP address, no localhost). Name it by digest (repo@sha256:…) to always run the same bits: an instance of a tag that is started later pulls whatever the tag points at then.", required: true, in: "body"},
			registryAuthParam,
			{name: "port", typ: "integer", desc: "Port the container listens on for HTTP (default 8080).", in: "body"},
			{name: "cpuMillis", typ: "integer", desc: "CPU of one instance in millicores: 250, 500, 1000 or 2000, up to the location's limit (default 250).", in: "body"},
			{name: "memoryMi", typ: "integer", desc: "Memory of one instance in MiB, from 256 up to the location's limit (default 256).", in: "body"},
			{name: "env", typ: "object", desc: "Environment variables {string: string}. Readable by every member of the project who may manage it; put credentials in secrets.", in: "body"},
			{name: "secrets", typ: "object", desc: "Secrets {string: string}: environment variables whose values are write-only. They are stored sealed and never returned by any call (get_function shows the names only). A function can still print them, and every member who may read the project reads its logs. The values land in this conversation.", in: "body"},
			{name: "command", typ: "array", desc: "Replaces the image's entrypoint (array of strings).", in: "body"},
			{name: "args", typ: "array", desc: "Replaces the image's arguments (array of strings). Passed as written: nothing is expanded.", in: "body"},
			{name: "minInstances", typ: "integer", desc: "Instances kept running when idle: 0 (scale to zero) or 1, if the location allows it. With 1 the function is billed all the time.", in: "body"},
			{name: "idleSeconds", typ: "integer", desc: "Seconds without requests before the function stops (default 300, within the location's bounds, usually 60 to 3600). The idle time is billed.", in: "body"},
			{name: "maxInstances", typ: "integer", desc: "Most instances the function is scaled to on load (default 1: one instance at most), up to the location's limit (usually 5). The project's quota counts the function at this many instances.", in: "body"},
			{name: "concurrency", typ: "integer", desc: "Requests in flight one instance is meant to take, 1-1000 (default 50): the function runs ceil(requests in flight / concurrency) instances, up to maxInstances. A target for scaling, not a limit per instance.", in: "body"},
		},
	},
	{
		name:   "update_function",
		desc:   "Change a function. Pass only what changes. A new image, port, size, env, command or args, a secret set or removed, or registry credentials set, replaced or removed, is rolled out as a new deployment (status DEPLOYING; a failed rollout, for instance one that cannot pull the image with new credentials, goes back to the previous deployment with its secrets and registry credentials, and sets lastError); minInstances, idleSeconds, maxInstances and concurrency apply without one. env, command and args replace the stored value as a whole; secrets are merged. Answers 409 while a deployment is in progress.",
		method: "PATCH",
		path:   "/api/v1/project/{id}/functions/{fn}",
		params: []param{
			projectID, functionID,
			{name: "image", typ: "string", desc: "Container image reference. An image in another registry than the stored registry credentials are for is refused: send registryAuth for that registry, or removeRegistryAuth, with it.", in: "body"},
			registryAuthParam, removeRegistryAuthParam,
			{name: "port", typ: "integer", desc: "Port the container listens on for HTTP.", in: "body"},
			{name: "cpuMillis", typ: "integer", desc: "CPU of one instance in millicores: 250, 500, 1000 or 2000, up to the location's limit.", in: "body"},
			{name: "memoryMi", typ: "integer", desc: "Memory of one instance in MiB.", in: "body"},
			{name: "env", typ: "object", desc: "Environment variables {string: string}; replaces all of them.", in: "body"},
			{name: "secrets", typ: "object", desc: "Secrets to set or remove {string: string or null}: a string sets or replaces that secret, null removes it, names not given stay as they are. Values are write-only and land in this conversation.", in: "body"},
			{name: "command", typ: "array", desc: "Replaces the image's entrypoint (array of strings).", in: "body"},
			{name: "args", typ: "array", desc: "Replaces the image's arguments (array of strings).", in: "body"},
			{name: "minInstances", typ: "integer", desc: "Instances kept running when idle: 0 or 1, if the location allows it.", in: "body"},
			{name: "idleSeconds", typ: "integer", desc: "Seconds without requests before the function stops.", in: "body"},
			{name: "maxInstances", typ: "integer", desc: "Most instances the function is scaled to on load (1 = one at most).", in: "body"},
			{name: "concurrency", typ: "integer", desc: "Requests in flight one instance is meant to take, 1-1000.", in: "body"},
		},
	},
	{
		name:   "delete_function",
		desc:   "Delete a function: its instances stop and its URL stops answering, for good (a URL is never handed out twice). Its last running interval is billed to this moment and nothing after it. Works also where functions were switched off.",
		method: "DELETE",
		path:   "/api/v1/project/{id}/functions/{fn}",
		params: []param{projectID, functionID},
	},
	{
		name:   "list_function_deployments",
		desc:   "List a function's deployments, newest first (the 10 newest are kept): sequence number, image and config, state (DEPLOYING, ACTIVE, SUPERSEDED or FAILED), the failure message and, for one that became ACTIVE, imageDigest: the digest it ran. Of secrets a deployment records the names (secretNames) and an opaque secretsVersion that changes whenever they do, never a value; of private registry credentials registryAuth {server, username, version}, never the password.",
		method: "GET",
		path:   "/api/v1/project/{id}/functions/{fn}/deployments",
		params: []param{projectID, functionID},
	},
	{
		name:   "rollback_function",
		desc:   "Roll a function back to an earlier deployment (state SUPERSEDED, from list_function_deployments). Its image, config and env are rolled out again as a new deployment; one that ran a tag is deployed by the digest it ran then, whatever the tag points at since. Secrets and private registry credentials are not versioned: the rollback runs with the function's current ones. When that deployment's image is in another registry than the current credentials are for, the rollback is refused unless registryAuth or removeRegistryAuth is sent with it.",
		method: "POST",
		path:   "/api/v1/project/{id}/functions/{fn}/rollback",
		params: []param{
			projectID, functionID,
			{name: "deploymentId", typ: "string", desc: "Id of the deployment to roll back to.", required: true, in: "body"},
			registryAuthParam, removeRegistryAuthParam,
		},
	},
	{
		name:   "function_action",
		desc:   "Suspend or resume a function. suspend stops its instances and keeps it stopped (requests get 503, nothing is billed); resume lets requests start it again, and starts the instance of a function that keeps one running: until it is ready the response is IDLE with instances 0 and a reason. suspend answers 409 while a deployment is in progress.",
		method: "POST",
		path:   "/api/v1/project/{id}/functions/{fn}/action",
		params: []param{
			projectID, functionID,
			{name: "action", typ: "string", desc: "\"suspend\" or \"resume\".", required: true, in: "body", enum: []string{"suspend", "resume"}},
		},
	},
	{
		name:   "get_function_logs",
		desc:   "Get the newest log lines (stdout and stderr, timestamped) of a function's running instances. Empty while it is scaled to zero. A function that prints its secrets shows them here, to every member who may read the project.",
		method: "GET",
		path:   "/api/v1/project/{id}/functions/{fn}/logs",
		params: []param{
			projectID, functionID,
			{name: "tail", typ: "integer", desc: "Number of lines (default 200, at most 5000).", in: "query"},
		},
	},
}
