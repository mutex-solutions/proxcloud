package mcp

// adminPricingTools drive the pricing surface (price plans, rules, billing configuration)
// through the internal /api/v1/admin routes. SigV4 api-key principals reach these because
// a verified hmac key resolves to SUPER_ADMIN in admin.adminContext.
var adminPricingTools = []toolDef{
	// ── price plans ──────────────────────────────────────────────────────────
	{
		name:   "list_price_plans",
		desc:   "List price plans (raw docs; accessMode PUBLIC applies to every billing profile).",
		method: "GET",
		path:   "/api/v1/admin/price-plan",
	},
	{
		name:   "get_price_plan",
		desc:   "Get a price plan by id.",
		method: "GET",
		path:   "/api/v1/admin/price-plan/{id}",
		params: []param{{name: "id", typ: "string", desc: "Price plan id.", required: true, in: "path"}},
	},
	{
		name:   "create_price_plan",
		desc:   "Create a price plan. accessMode defaults to PUBLIC when omitted.",
		method: "POST",
		path:   "/api/v1/admin/price-plan",
		params: []param{
			{name: "name", typ: "string", desc: "Plan name.", required: true, in: "body"},
			{name: "enabled", typ: "boolean", desc: "Disabled plans are skipped during rating.", in: "body"},
			{name: "accessMode", typ: "string", desc: "PUBLIC (applies to everyone) or SCOPED (assigned per billing profile).", in: "body"},
			{name: "serviceProviders", typ: "array", desc: "Optional scope list of {serviceId} objects; empty = every provider.", in: "body"},
		},
	},
	{
		name:   "update_price_plan",
		desc:   "Update a price plan's name/enabled/accessMode/serviceProviders (name and serviceProviders are overwritten; omitted accessMode is kept).",
		method: "PUT",
		path:   "/api/v1/admin/price-plan/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Price plan id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Plan name.", required: true, in: "body"},
			{name: "enabled", typ: "boolean", desc: "Enable/disable the plan.", in: "body"},
			{name: "accessMode", typ: "string", desc: "PUBLIC or SCOPED; omitted = unchanged.", in: "body"},
			{name: "serviceProviders", typ: "array", desc: "Scope list of {serviceId} objects; omitted = cleared.", in: "body"},
		},
	},

	// ── price plan rules ─────────────────────────────────────────────────────
	{
		name:   "list_price_plan_rules",
		desc:   "List a price plan's rules.",
		method: "GET",
		path:   "/api/v1/admin/price-plan/{id}/rule",
		params: []param{{name: "id", typ: "string", desc: "Price plan id.", required: true, in: "path"}},
	},
	{
		name:   "get_price_plan_rule",
		desc:   "Get a single pricing rule by id.",
		method: "GET",
		path:   "/api/v1/admin/price-plan/rule/{id}",
		params: []param{{name: "id", typ: "string", desc: "Rule id.", required: true, in: "path"}},
	},
	{
		name:   "create_price_plan_rule",
		desc:   "Create a pricing rule. prices = [{attributeName, tiers:[{from?,to?,value}]}] (decimal strings); filters = [{attributeName, operator (eq/neq/in/gt/...), value}]. Example GPU rule: filter gpu_model eq nvidia-a6000 + price gpu_count.",
		method: "POST",
		path:   "/api/v1/admin/price-plan/rule",
		params: []param{
			{name: "name", typ: "string", desc: "Rule name.", required: true, in: "body"},
			{name: "timeUnit", typ: "string", desc: "minute | hour | month.", required: true, in: "body"},
			{name: "resourceType", typ: "string", desc: "instance | instance_traffic | volume | floating_ip | load_balancer.", required: true, in: "body"},
			{name: "pricePlanId", typ: "string", desc: "Owning price plan id.", required: true, in: "body"},
			{name: "applyMethod", typ: "string", desc: "ADD_TO_TOTAL (default) or OVERWRITE_TOTAL.", in: "body"},
			{name: "prices", typ: "array", desc: "Priced attributes: [{attributeName, tiers:[{from?,to?,value}]}].", in: "body"},
			{name: "filters", typ: "array", desc: "Match gates: [{attributeName, operator, value}].", in: "body"},
			{name: "modifiers", typ: "array", desc: "Conditional adjustments (advanced; usually []).", in: "body"},
		},
	},
	{
		name:   "update_price_plan_rule",
		desc:   "Update a pricing rule (same fields as create; supplied fields overwrite).",
		method: "PUT",
		path:   "/api/v1/admin/price-plan/rule/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Rule id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Rule name.", required: true, in: "body"},
			{name: "timeUnit", typ: "string", desc: "minute | hour | month.", required: true, in: "body"},
			{name: "resourceType", typ: "string", desc: "Billing resource type.", required: true, in: "body"},
			{name: "pricePlanId", typ: "string", desc: "Owning price plan id.", in: "body"},
			{name: "applyMethod", typ: "string", desc: "ADD_TO_TOTAL or OVERWRITE_TOTAL.", in: "body"},
			{name: "prices", typ: "array", desc: "Priced attributes.", in: "body"},
			{name: "filters", typ: "array", desc: "Match gates.", in: "body"},
			{name: "modifiers", typ: "array", desc: "Conditional adjustments.", in: "body"},
		},
	},
	{
		name:   "delete_price_plan_rule",
		desc:   "Delete a pricing rule.",
		method: "DELETE",
		path:   "/api/v1/admin/price-plan/rule/{id}",
		params: []param{{name: "id", typ: "string", desc: "Rule id.", required: true, in: "path"}},
	},
	{
		name:   "list_billing_resource_types",
		desc:   "List the billable resource types and their rateable attributes (what rules can filter/price on — includes gpu_count/gpu_model on instance).",
		method: "GET",
		path:   "/api/v1/admin/price-plan/resource-types",
	},
	{
		name:   "get_price_plan_rule_usage",
		desc:   "Report how much a rule has charged (usage aggregation).",
		method: "GET",
		path:   "/api/v1/admin/price-plan/rule/{id}/usage",
		params: []param{{name: "id", typ: "string", desc: "Rule id.", required: true, in: "path"}},
	},

	// ── billing configuration ────────────────────────────────────────────────
	{
		name:   "get_billing_configuration",
		desc:   "Get the current (default) billing configuration — id, base currency, promotion codes flag.",
		method: "GET",
		path:   "/api/v1/admin/billing/configuration/current",
	},
	{
		name:   "create_billing_configuration",
		desc:   "Create a billing configuration (fresh installs: set the base currency, e.g. USD).",
		method: "POST",
		path:   "/api/v1/admin/billing/configuration",
		params: []param{
			{name: "baseCurrency", typ: "string", desc: "ISO currency code prices are stored in (e.g. USD).", required: true, in: "body"},
			{name: "defaultConfiguration", typ: "boolean", desc: "Mark as the default configuration (usually true).", in: "body"},
			{name: "promotionCodesEnabled", typ: "boolean", desc: "Enable promotion codes.", in: "body"},
			{name: "name", typ: "string", desc: "Optional configuration name.", in: "body"},
			{name: "settings", typ: "object", desc: "Optional settings block (time-unit limits etc.).", in: "body"},
		},
	},
	{
		name:   "update_billing_configuration",
		desc:   "Update a billing configuration. WARNING: overwrites ALL 13 mutable fields — an omitted field becomes null. Read it first (get_billing_configuration returns a partial view; prefer create-once + targeted edits).",
		method: "PUT",
		path:   "/api/v1/admin/billing/configuration/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Billing configuration id.", required: true, in: "path"},
			{name: "baseCurrency", typ: "string", desc: "Base currency (required by validation).", required: true, in: "body"},
			{name: "defaultConfiguration", typ: "boolean", desc: "Default-configuration flag.", in: "body"},
			{name: "promotionCodesEnabled", typ: "boolean", desc: "Promotion codes flag.", in: "body"},
			{name: "name", typ: "string", desc: "Configuration name.", in: "body"},
			{name: "mailGatewayId", typ: "string", desc: "Mail gateway integration id.", in: "body"},
			{name: "invoiceGatewayId", typ: "string", desc: "Invoice gateway integration id.", in: "body"},
			{name: "address", typ: "object", desc: "Operator address block.", in: "body"},
			{name: "company", typ: "object", desc: "Operator company block.", in: "body"},
			{name: "settings", typ: "object", desc: "Settings block.", in: "body"},
			{name: "provisioningSettings", typ: "object", desc: "Provisioning settings block.", in: "body"},
			{name: "autoActivationFlow", typ: "object", desc: "Auto-activation flow block.", in: "body"},
			{name: "suspensionConfiguration", typ: "object", desc: "Suspension configuration block.", in: "body"},
			{name: "savingsContractNotificationConfig", typ: "object", desc: "Savings notification block.", in: "body"},
		},
	},
	{
		name:   "list_currencies",
		desc:   "List the supported currency catalog (currency_code/currency_name per country).",
		method: "GET",
		path:   "/api/v1/admin/billing/configuration/currencies",
	},
	{
		name:   "list_unpriced_flavors",
		desc:   "The zero-billing guard: live flavors of a cloud provider that match no enabled public price rule (reason 'no rule') or whose GPUs match no GPU-aware rule (reason 'no gpu rule').",
		method: "GET",
		path:   "/api/v1/admin/service/{id}/unpriced-flavors",
		params: []param{{name: "id", typ: "string", desc: "Cloud provider (external service) id.", required: true, in: "path"}},
	},
}
