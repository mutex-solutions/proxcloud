package mcp

// adminPlatformTools drive platform + cloud-provider configuration through the internal
// /api/v1/admin routes: platform configuration (branding, quotas, regions), provider
// connection/services/quota/features, GPU capacity, flavor categories, custom menu,
// message templates and taxes.
var adminPlatformTools = []toolDef{
	// ── platform configuration ───────────────────────────────────────────────
	{
		name:   "get_platform_configuration",
		desc:   "Get the current platform configuration (branding, date format, project/organization provisioning quotas, regions display config). A fresh install auto-creates the default config on first read.",
		method: "GET",
		path:   "/api/v1/admin/platform-configuration/current",
	},
	{
		name:   "update_platform_configuration",
		desc:   "Update the platform configuration. WARNING: the body REPLACES the stored document — call get_platform_configuration first and send the full document back with your edits (fields: name, language, branding{name,color,logo,faviconUrl}, dateConfiguration{dateFormat}, projectProvisioningQuota{enabled,limit}, organizationProvisioningQuota{enabled,limit}, regions[], mailGatewayId, ...).",
		method: "PUT",
		path:   "/api/v1/admin/platform-configuration/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Platform configuration id (from get_platform_configuration).", required: true, in: "path"},
			{name: "configuration", typ: "object", desc: "The FULL configuration document to store (read-modify-write).", required: true, in: "rawbody"},
		},
	},
	{
		name:   "set_platform_regions",
		desc:   "Replace the platform regions display config — the ordered region list the client UI offers. Body: [{serviceId, region, order}].",
		method: "PUT",
		path:   "/api/v1/admin/platform-configuration/{id}/regions",
		params: []param{
			{name: "id", typ: "string", desc: "Platform configuration id.", required: true, in: "path"},
			{name: "regions", typ: "array", desc: "Full region list: [{serviceId, region, order}].", required: true, in: "rawbody"},
		},
	},

	// ── cloud providers (external services) ──────────────────────────────────
	{
		name:   "list_cloud_providers",
		desc:   "List cloud providers (external services) with their full config (regions, per-region service toggles, quota); secrets are stripped.",
		method: "GET",
		path:   "/api/v1/admin/service",
	},
	{
		name:   "get_cloud_provider",
		desc:   "Get one cloud provider by id, full config (secrets stripped).",
		method: "GET",
		path:   "/api/v1/admin/service/{id}",
		params: []param{{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"}},
	},
	{
		name:   "update_cloud_provider",
		desc:   "Update a cloud provider. config/secret are MERGED wholesale into the stored doc (read the provider first). Per-region service toggles live at config.services = {\"<slug>\": {\"<region>\": bool}} (slugs: compute, network, image, volumev3, load-balancer, key-manager, object-store, dns, orchestration, sharev2, ...) — the sync job and client menus gate on them. Secret fields left blank are kept.",
		method: "PUT",
		path:   "/api/v1/admin/service/{id}/update",
		params: []param{
			{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Display name.", in: "body"},
			{name: "status", typ: "string", desc: "public | private | disabled.", in: "body"},
			{name: "defaultPricePlan", typ: "string", desc: "Default price plan id.", in: "body"},
			{name: "config", typ: "object", desc: "Config keys to merge (e.g. {services: {dns: {RegionOne: false}}}).", in: "body"},
			{name: "secret", typ: "object", desc: "Secret keys to merge (blank values keep the stored ones). Never returned.", in: "body"},
		},
	},
	{
		name:   "discover_cloud_provider",
		desc:   "Re-read the provider's Keystone catalog and merge discovered regions + services onto its config (the Sync services & regions button).",
		method: "POST",
		path:   "/api/v1/admin/service/{id}/discover",
		params: []param{{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"}},
	},
	{
		name:   "set_provider_default_quota",
		desc:   "Replace the provider's default OpenStack quota config (config.provisioning.quota) — stored config only, not pushed to the cloud. Body example: {instances: 10, cores: 20, ram: 51200, volumes: 10, gigabytes: 500, floatingips: 3, ...}.",
		method: "PUT",
		path:   "/api/v1/admin/service/{id}/quota",
		params: []param{
			{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"},
			{name: "quota", typ: "object", desc: "The full quota object to store (replaces the stored one).", required: true, in: "rawbody"},
		},
	},
	{
		name:   "set_provider_features",
		desc:   "Replace the provider's feature config (config.features) — read the provider first; the object is stored wholesale.",
		method: "PUT",
		path:   "/api/v1/admin/service/{id}/features",
		params: []param{
			{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"},
			{name: "features", typ: "object", desc: "The full features object.", required: true, in: "rawbody"},
		},
	},
	{
		name:   "set_gnocchi_granularity",
		desc:   "Set the provider's Gnocchi metric granularity in seconds (positive integer).",
		method: "PUT",
		path:   "/api/v1/admin/service/{id}/gnocchi-granularity",
		params: []param{
			{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"},
			{name: "granularity", typ: "integer", desc: "Granularity in seconds.", required: true, in: "body"},
		},
	},
	{
		name:   "set_metrics_config",
		desc:   "Set the provider's usage-metrics source for traffic billing: gnocchi (default), prometheus, or none (skip metrics entirely). config.metrics is MERGED — a source-only toggle keeps the stored prometheus connection config. prometheus = {url: base up to /api/v1 (e.g. https://mimir.example/prometheus), schema: libvirt-exporter|ceilometer-pushgateway|ceilometer-exporter, headers: extra request headers (e.g. {\"X-Scope-OrgID\": \"tenant\"} for Mimir), basicUser, insecureTls, caCert, timeoutSeconds}. Credentials go in prometheusAuth = {basicPassword, bearerToken} — encrypted at rest, never returned; blank keeps the stored value, \"-\" clears it. Authorization headers and URL userinfo are rejected (use prometheusAuth).",
		method: "PUT",
		path:   "/api/v1/admin/service/{id}/metrics-config",
		params: []param{
			{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"},
			{name: "source", typ: "string", desc: "gnocchi | prometheus | none.", required: true, in: "body"},
			{name: "prometheus", typ: "object", desc: "Prometheus connection config (see tool description).", in: "body"},
			{name: "prometheusAuth", typ: "object", desc: "{basicPassword, bearerToken} — stored encrypted, never returned.", in: "body"},
		},
	},
	{
		name:   "test_metrics_config",
		desc:   "Live-probe the provider's configured prometheus metrics source (read-only): liveness, traffic-series count over the last hour (proves the schema matches the endpoint), and the same count at month start (proves retention covers the billing window). Returns {ok, trafficSeries, monthStartSeries, warnings}.",
		method: "POST",
		path:   "/api/v1/admin/service/{id}/metrics-test",
		params: []param{{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"}},
	},
	{
		name:   "get_gpu_capacity",
		desc:   "Cluster-wide GPU capacity per model from Placement, per region: [{region, gpus: [{name, total, inUse}]}]. Model names are the shared alias vocabulary (nvidia-a6000, nvidia-pro-6000, ...).",
		method: "GET",
		path:   "/api/v1/admin/service/{id}/gpu-info",
		params: []param{{name: "id", typ: "string", desc: "Cloud provider id.", required: true, in: "path"}},
	},
	{
		name:   "list_live_flavors",
		desc:   "Live Nova flavors across the provider's regions (id, name, vcpus, ram, disk, extra_specs — GPU flavors carry pci_passthrough:alias).",
		method: "GET",
		path:   "/api/v1/admin/flavor-categories/flavors",
	},

	// ── flavor categories (the curated Hardware groups in the create-server flow) ──
	{
		name:   "list_flavor_categories",
		desc:   "List flavor categories (the curated hardware groups the client create-server flow shows).",
		method: "GET",
		path:   "/api/v1/admin/flavor-categories",
	},
	{
		name:   "get_flavor_category",
		desc:   "Get a flavor category by id.",
		method: "GET",
		path:   "/api/v1/admin/flavor-categories/{id}",
		params: []param{{name: "id", typ: "string", desc: "Flavor category id.", required: true, in: "path"}},
	},
	{
		name:   "create_flavor_category",
		desc:   "Create a flavor category. flavors = [{flavorName, ...}] entries matched to live nova flavors by name; flavorAttributes = display attribute rows.",
		method: "POST",
		path:   "/api/v1/admin/flavor-categories",
		params: []param{
			{name: "name", typ: "string", desc: "Category name (e.g. GPU — A6000).", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Category description.", in: "body"},
			{name: "orderNumber", typ: "integer", desc: "Sort order in the client UI.", in: "body"},
			{name: "bareMetal", typ: "boolean", desc: "Bare-metal category flag.", in: "body"},
			{name: "kubernetesFlavorCategory", typ: "boolean", desc: "Kubernetes-flavor category flag.", in: "body"},
			{name: "flavors", typ: "array", desc: "Category entries: [{flavorName, ...}].", in: "body"},
			{name: "flavorAttributes", typ: "array", desc: "Display attribute rows.", in: "body"},
		},
	},
	{
		name:   "update_flavor_category",
		desc:   "Update a flavor category (same fields as create; supplied fields overwrite).",
		method: "PUT",
		path:   "/api/v1/admin/flavor-categories/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Flavor category id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Category name.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Category description.", in: "body"},
			{name: "orderNumber", typ: "integer", desc: "Sort order.", in: "body"},
			{name: "bareMetal", typ: "boolean", desc: "Bare-metal flag.", in: "body"},
			{name: "kubernetesFlavorCategory", typ: "boolean", desc: "Kubernetes flag.", in: "body"},
			{name: "flavors", typ: "array", desc: "Category entries.", in: "body"},
			{name: "flavorAttributes", typ: "array", desc: "Display attribute rows.", in: "body"},
		},
	},
	{
		name:   "delete_flavor_category",
		desc:   "Delete a flavor category.",
		method: "DELETE",
		path:   "/api/v1/admin/flavor-categories/{id}",
		params: []param{{name: "id", typ: "string", desc: "Flavor category id.", required: true, in: "path"}},
	},

	// ── custom menu (client "More" links) ────────────────────────────────────
	{
		name:   "list_menu_items",
		desc:   "List the custom client-menu items (the More section links).",
		method: "GET",
		path:   "/api/v1/admin/menu",
	},
	{
		name:   "create_menu_item",
		desc:   "Create a custom client-menu item. url may carry placeholders (see get_menu_placeholders); renderMode IFRAME or NEW_TAB.",
		method: "POST",
		path:   "/api/v1/admin/menu",
		params: []param{
			{name: "displayName", typ: "string", desc: "Menu label.", required: true, in: "body"},
			{name: "url", typ: "string", desc: "Target URL (placeholders like {{project.id}} allowed).", required: true, in: "body"},
			{name: "icon", typ: "string", desc: "Icon name.", in: "body"},
			{name: "renderMode", typ: "string", desc: "IFRAME or NEW_TAB.", in: "body"},
			{name: "order", typ: "integer", desc: "Sort order.", in: "body"},
		},
	},
	{
		name:   "update_menu_item",
		desc:   "Update a custom menu item (same fields as create).",
		method: "PUT",
		path:   "/api/v1/admin/menu/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Menu item id.", required: true, in: "path"},
			{name: "displayName", typ: "string", desc: "Menu label.", required: true, in: "body"},
			{name: "url", typ: "string", desc: "Target URL.", required: true, in: "body"},
			{name: "icon", typ: "string", desc: "Icon name.", in: "body"},
			{name: "renderMode", typ: "string", desc: "IFRAME or NEW_TAB.", in: "body"},
			{name: "order", typ: "integer", desc: "Sort order.", in: "body"},
		},
	},
	{
		name:   "delete_menu_item",
		desc:   "Delete a custom menu item.",
		method: "DELETE",
		path:   "/api/v1/admin/menu/{id}",
		params: []param{{name: "id", typ: "string", desc: "Menu item id.", required: true, in: "path"}},
	},
	{
		name:   "get_menu_placeholders",
		desc:   "The URL placeholders custom menu items may use ({{project.id}}, {{user.email}}, ...).",
		method: "GET",
		path:   "/api/v1/admin/menu/placeholders",
	},

	// ── message templates ────────────────────────────────────────────────────
	{
		name:   "list_message_templates",
		desc:   "List the platform message templates (system mail bodies).",
		method: "GET",
		path:   "/api/v1/admin/message-templates",
	},
	{
		name:   "get_message_template",
		desc:   "Get a message template by id.",
		method: "GET",
		path:   "/api/v1/admin/message-templates/{id}",
		params: []param{{name: "id", typ: "string", desc: "Template id.", required: true, in: "path"}},
	},
	{
		name:   "update_message_template",
		desc:   "Update a message template — only messageTitle, messageBody and disabled are applied.",
		method: "PUT",
		path:   "/api/v1/admin/message-templates/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Template id.", required: true, in: "path"},
			{name: "messageTitle", typ: "string", desc: "Mail subject template.", in: "body"},
			{name: "messageBody", typ: "string", desc: "Mail body template.", in: "body"},
			{name: "disabled", typ: "boolean", desc: "Disable sending for this template.", in: "body"},
		},
	},

	// ── taxes ────────────────────────────────────────────────────────────────
	{
		name:   "list_taxes",
		desc:   "List the configured tax rates.",
		method: "GET",
		path:   "/api/v1/admin/tax",
	},
	{
		name:   "create_tax",
		desc:   "Create a tax rate. rateLevels = [{level, percentage}] (whole percents); level is required.",
		method: "POST",
		path:   "/api/v1/admin/tax",
		params: []param{
			{name: "name", typ: "string", desc: "Tax name (e.g. VAT).", required: true, in: "body"},
			{name: "country", typ: "string", desc: "Country code the tax applies to.", in: "body"},
			{name: "state", typ: "string", desc: "State/region (optional).", in: "body"},
			{name: "level", typ: "integer", desc: "Tax level (required by validation).", required: true, in: "body"},
			{name: "accessMode", typ: "string", desc: "PUBLIC or SCOPED.", in: "body"},
			{name: "rateLevels", typ: "array", desc: "[{level, percentage}] whole percents.", in: "body"},
			{name: "startDate", typ: "string", desc: "Validity start (RFC3339).", in: "body"},
			{name: "endDate", typ: "string", desc: "Validity end (RFC3339).", in: "body"},
			{name: "startDateEnabled", typ: "boolean", desc: "Honor startDate.", in: "body"},
			{name: "endDateEnabled", typ: "boolean", desc: "Honor endDate.", in: "body"},
		},
	},
	{
		name:   "update_tax",
		desc:   "Update a tax rate (same fields as create; state is immutable).",
		method: "PUT",
		path:   "/api/v1/admin/tax/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Tax id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Tax name.", required: true, in: "body"},
			{name: "country", typ: "string", desc: "Country code.", in: "body"},
			{name: "level", typ: "integer", desc: "Tax level.", required: true, in: "body"},
			{name: "accessMode", typ: "string", desc: "PUBLIC or SCOPED.", in: "body"},
			{name: "rateLevels", typ: "array", desc: "[{level, percentage}].", in: "body"},
			{name: "startDate", typ: "string", desc: "Validity start.", in: "body"},
			{name: "endDate", typ: "string", desc: "Validity end.", in: "body"},
			{name: "startDateEnabled", typ: "boolean", desc: "Honor startDate.", in: "body"},
			{name: "endDateEnabled", typ: "boolean", desc: "Honor endDate.", in: "body"},
		},
	},
	{
		name:   "delete_tax",
		desc:   "Delete a tax rate (400 when a SCOPED rate is still referenced).",
		method: "DELETE",
		path:   "/api/v1/admin/tax/{id}",
		params: []param{{name: "id", typ: "string", desc: "Tax id.", required: true, in: "path"}},
	},
}
