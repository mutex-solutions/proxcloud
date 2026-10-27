package mcp

// adminBillingOpsTools drive billing operations + observability through the internal
// /api/v1/admin routes: stats, audit search, transactions (incl. refund / bank-transfer
// settlement), savings plans, promotion codes, price-adjustment rules and integrations.
var adminBillingOpsTools = []toolDef{
	// ── observability ────────────────────────────────────────────────────────
	{
		name:   "get_admin_stats",
		desc:   "Platform dashboard stats: user/project/resource/transaction counts, setup flags (cloud provider, billing, branding, mail, price plan) and 12-month insights.",
		method: "GET",
		path:   "/api/v1/admin/stats",
	},
	{
		name:   "search_audit_log",
		desc:   "Search the admin audit log (cursor-paginated, newest first).",
		method: "GET",
		path:   "/api/v1/admin/audit",
		params: []param{
			{name: "limit", typ: "integer", desc: "Page size.", in: "query"},
			{name: "after", typ: "string", desc: "Cursor: events after this marker (mutually exclusive with before).", in: "query"},
			{name: "before", typ: "string", desc: "Cursor: events before this marker.", in: "query"},
			{name: "organizationId", typ: "string", desc: "Filter by organization.", in: "query"},
			{name: "projectId", typ: "string", desc: "Filter by project.", in: "query"},
			{name: "resourceType", typ: "string", desc: "Filter by resource type (e.g. PRICE_PLAN).", in: "query"},
			{name: "resourceId", typ: "string", desc: "Filter by resource id.", in: "query"},
			{name: "actorId", typ: "string", desc: "Filter by actor (sub or api-key id).", in: "query"},
			{name: "action", typ: "string", desc: "Filter by action (CREATE/UPDATE/DELETE).", in: "query"},
			{name: "outcome", typ: "string", desc: "Filter by outcome.", in: "query"},
			{name: "search", typ: "string", desc: "Free-text search.", in: "query"},
			{name: "from", typ: "string", desc: "RFC3339 window start.", in: "query"},
			{name: "to", typ: "string", desc: "RFC3339 window end.", in: "query"},
		},
	},

	// ── transactions ─────────────────────────────────────────────────────────
	{
		name:   "list_account_credit_transactions",
		desc:   "List all account-credit (deposit) transactions.",
		method: "GET",
		path:   "/api/v1/admin/account-credit-transactions",
	},
	{
		name:   "list_collect_transactions",
		desc:   "List all collect (bill payment) transactions.",
		method: "GET",
		path:   "/api/v1/admin/collect-transactions",
	},
	{
		name:   "list_billing_profile_transactions",
		desc:   "List one billing profile's transactions (collect + account-credit merged).",
		method: "GET",
		path:   "/api/v1/admin/transactions/{billingProfileId}",
		params: []param{{name: "billingProfileId", typ: "string", desc: "Billing profile id.", required: true, in: "path"}},
	},
	{
		name:   "refund_transaction",
		desc:   "Refund a SUCCESS deposit: refunds the payment-gateway PaymentIntent and voids its account credit (transaction → REFUNDED).",
		method: "POST",
		path:   "/api/v1/admin/account-credit-transactions/refund/{id}",
		params: []param{{name: "id", typ: "string", desc: "Account-credit transaction id.", required: true, in: "path"}},
	},
	{
		name:   "approve_bank_transfer",
		desc:   "Approve a pending bank transfer — settles the deposit and mints the account credit.",
		method: "POST",
		path:   "/api/v1/admin/bank-transfer/{id}/approve",
		params: []param{{name: "id", typ: "string", desc: "Bank transfer id.", required: true, in: "path"}},
	},
	{
		name:   "reject_bank_transfer",
		desc:   "Reject a pending bank transfer (its transaction becomes FAILED; no credit is minted).",
		method: "POST",
		path:   "/api/v1/admin/bank-transfer/{id}/reject",
		params: []param{{name: "id", typ: "string", desc: "Bank transfer id.", required: true, in: "path"}},
	},

	// ── savings plans ────────────────────────────────────────────────────────
	{
		name:   "list_savings_plans",
		desc:   "List savings plans.",
		method: "GET",
		path:   "/api/v1/admin/savings-plans",
	},
	{
		name:   "create_savings_plan",
		desc:   "Create a savings plan. savingSchedule = [{durationMonths, maxAmount, noUpfrontTiers:[{startAmount,discount}], upfrontTiers:[...]}].",
		method: "POST",
		path:   "/api/v1/admin/savings-plans",
		params: []param{
			{name: "name", typ: "string", desc: "Plan name.", required: true, in: "body"},
			{name: "available", typ: "boolean", desc: "Purchasable by clients.", in: "body"},
			{name: "description", typ: "string", desc: "Description.", in: "body"},
			{name: "targets", typ: "array", desc: "Target resource selectors.", in: "body"},
			{name: "savingSchedule", typ: "array", desc: "Duration/discount schedule.", in: "body"},
			{name: "accessMode", typ: "string", desc: "PUBLIC or SCOPED.", in: "body"},
			{name: "billingProfiles", typ: "array", desc: "Scoped billing profile ids.", in: "body"},
		},
	},
	{
		name:   "update_savings_plan",
		desc:   "Update a savings plan (same fields as create).",
		method: "PUT",
		path:   "/api/v1/admin/savings-plans/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Savings plan id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Plan name.", required: true, in: "body"},
			{name: "available", typ: "boolean", desc: "Purchasable by clients.", in: "body"},
			{name: "description", typ: "string", desc: "Description.", in: "body"},
			{name: "targets", typ: "array", desc: "Target resource selectors.", in: "body"},
			{name: "savingSchedule", typ: "array", desc: "Duration/discount schedule.", in: "body"},
			{name: "accessMode", typ: "string", desc: "PUBLIC or SCOPED.", in: "body"},
			{name: "billingProfiles", typ: "array", desc: "Scoped billing profile ids.", in: "body"},
		},
	},
	{
		name:   "delete_savings_plan",
		desc:   "Delete a savings plan.",
		method: "DELETE",
		path:   "/api/v1/admin/savings-plans/{id}",
		params: []param{{name: "id", typ: "string", desc: "Savings plan id.", required: true, in: "path"}},
	},

	// ── promotion codes ──────────────────────────────────────────────────────
	{
		name:   "list_promotion_codes",
		desc:   "List promotion codes.",
		method: "GET",
		path:   "/api/v1/admin/promotion-codes",
	},
	{
		name:   "create_promotion_code",
		desc:   "Create a promotion code (amount as a decimal string; validity windows RFC3339).",
		method: "POST",
		path:   "/api/v1/admin/promotion-codes",
		params: []param{
			{name: "code", typ: "string", desc: "The redeemable code.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Description.", in: "body"},
			{name: "amount", typ: "string", desc: "Credit amount (decimal string).", required: true, in: "body"},
			{name: "creditValidityDuration", typ: "object", desc: "Credit validity duration block.", in: "body"},
			{name: "validFrom", typ: "string", desc: "Redeemable from (RFC3339).", in: "body"},
			{name: "validUntil", typ: "string", desc: "Redeemable until (RFC3339).", in: "body"},
			{name: "targetOrganizationIds", typ: "array", desc: "Restrict to these organizations.", in: "body"},
			{name: "status", typ: "string", desc: "Code status.", in: "body"},
		},
	},
	{
		name:   "update_promotion_code",
		desc:   "Update a promotion code (same fields as create).",
		method: "PUT",
		path:   "/api/v1/admin/promotion-codes/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Promotion code id.", required: true, in: "path"},
			{name: "code", typ: "string", desc: "The redeemable code.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Description.", in: "body"},
			{name: "amount", typ: "string", desc: "Credit amount (decimal string).", in: "body"},
			{name: "creditValidityDuration", typ: "object", desc: "Credit validity duration block.", in: "body"},
			{name: "validFrom", typ: "string", desc: "Redeemable from.", in: "body"},
			{name: "validUntil", typ: "string", desc: "Redeemable until.", in: "body"},
			{name: "targetOrganizationIds", typ: "array", desc: "Restrict to these organizations.", in: "body"},
			{name: "status", typ: "string", desc: "Code status.", in: "body"},
		},
	},
	{
		name:   "delete_promotion_code",
		desc:   "Delete a promotion code.",
		method: "DELETE",
		path:   "/api/v1/admin/promotion-codes/{id}",
		params: []param{{name: "id", typ: "string", desc: "Promotion code id.", required: true, in: "path"}},
	},

	// ── price adjustment rules (volume discounts / surcharges) ───────────────
	{
		name:   "list_price_adjustment_rules",
		desc:   "List a price plan's adjustment rules (tiered discounts/surcharges on the rated amount).",
		method: "GET",
		path:   "/api/v1/admin/price-adjustment-rules/price-plan/{pricePlanId}",
		params: []param{{name: "pricePlanId", typ: "string", desc: "Price plan id.", required: true, in: "path"}},
	},
	{
		name:   "create_price_adjustment_rule",
		desc:   "Create a price-adjustment rule. tiers = [{startAmount, modifier...}] — each tier pairs a spend threshold with an add/subtract modifier (percentage or flat).",
		method: "POST",
		path:   "/api/v1/admin/price-adjustment-rules",
		params: []param{
			{name: "name", typ: "string", desc: "Rule name.", required: true, in: "body"},
			{name: "enabled", typ: "boolean", desc: "Enabled flag.", in: "body"},
			{name: "description", typ: "string", desc: "Description.", in: "body"},
			{name: "pricePlanId", typ: "string", desc: "Owning price plan id.", required: true, in: "body"},
			{name: "targets", typ: "array", desc: "Target resource selectors (empty = whole bill).", in: "body"},
			{name: "tiers", typ: "array", desc: "Spend tiers with modifiers.", in: "body"},
		},
	},
	{
		name:   "update_price_adjustment_rule",
		desc:   "Update a price-adjustment rule (pricePlanId is immutable).",
		method: "PUT",
		path:   "/api/v1/admin/price-adjustment-rules/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Rule id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Rule name.", required: true, in: "body"},
			{name: "enabled", typ: "boolean", desc: "Enabled flag.", in: "body"},
			{name: "description", typ: "string", desc: "Description.", in: "body"},
			{name: "targets", typ: "array", desc: "Target resource selectors.", in: "body"},
			{name: "tiers", typ: "array", desc: "Spend tiers with modifiers.", in: "body"},
		},
	},
	{
		name:   "delete_price_adjustment_rule",
		desc:   "Delete a price-adjustment rule.",
		method: "DELETE",
		path:   "/api/v1/admin/price-adjustment-rules/{id}",
		params: []param{{name: "id", typ: "string", desc: "Rule id.", required: true, in: "path"}},
	},

	// ── integrations ─────────────────────────────────────────────────────────
	{
		name:   "list_integrations",
		desc:   "List third-party integrations (payment gateways, SMTP, ...); secrets are never returned.",
		method: "GET",
		path:   "/api/v1/admin/integrations",
	},
	{
		name:   "create_integration",
		desc:   "Create a third-party integration (e.g. thirdParty STRIPE or SMTP). secret is WRITE-ONLY and never returned.",
		method: "POST",
		path:   "/api/v1/admin/integrations",
		params: []param{
			{name: "name", typ: "string", desc: "Display name.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Description.", in: "body"},
			{name: "thirdParty", typ: "string", desc: "Integration type key (STRIPE, SMTP, BANK_TRANSFER, BUILT_IN_INVOICING, ...).", required: true, in: "body"},
			{name: "config", typ: "object", desc: "Non-secret config block (shape depends on thirdParty).", in: "body"},
			{name: "secret", typ: "object", desc: "Secret block (write-only; omit to keep on update).", in: "body"},
			{name: "metadata", typ: "object", desc: "Free-form metadata.", in: "body"},
		},
	},
	{
		name:   "update_integration",
		desc:   "Update an integration. The stored secret is kept unless a complete new secret block is supplied.",
		method: "PUT",
		path:   "/api/v1/admin/integrations/{id}",
		params: []param{
			{name: "id", typ: "string", desc: "Integration id.", required: true, in: "path"},
			{name: "name", typ: "string", desc: "Display name.", required: true, in: "body"},
			{name: "description", typ: "string", desc: "Description.", in: "body"},
			{name: "thirdParty", typ: "string", desc: "Integration type key.", in: "body"},
			{name: "config", typ: "object", desc: "Non-secret config block.", in: "body"},
			{name: "secret", typ: "object", desc: "New secret block (omit to keep the stored one).", in: "body"},
			{name: "metadata", typ: "object", desc: "Free-form metadata.", in: "body"},
		},
	},
	{
		name:   "delete_integration",
		desc:   "Delete a third-party integration.",
		method: "DELETE",
		path:   "/api/v1/admin/integrations/{id}",
		params: []param{{name: "id", typ: "string", desc: "Integration id.", required: true, in: "path"}},
	},
}
