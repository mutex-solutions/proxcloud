# MCP Server

ProxCloud bundles a [Model Context Protocol](https://modelcontextprotocol.io) server, so AI agents — Claude Code, Claude Desktop, or any MCP-capable client — can drive the platform through curated tools rather than raw HTTP.

- **Endpoint:** `https://<api-host>/mcp` (Streamable HTTP, stateless — safe behind multiple API replicas)
- **Toolsets:** the tools you're offered depend on who you are. Admin principals get the admin toolset (users, organizations, projects, billing profiles, credits); end users signing in with a portal account get the client toolset (their own projects and cloud resources).

Every tool call runs against the very same REST endpoints documented in this reference — same permissions, same validation, same audit trail. MCP opens no privileged side door.

## Authentication

There are two ways in.

### 1. OAuth sign-in (interactive)

Add the server with no credentials and let your MCP client run the standard OAuth flow:

```bash
claude mcp add --transport http proxcloud https://<api-host>/mcp
```

On first use the server replies `401` with an RFC 9728 resource-metadata document pointing at the ProxCloud identity provider. Your MCP client discovers it, registers itself via dynamic client registration, opens a browser window, and redirects back to a local port with an authorization code (PKCE). No server-side setup required.

- Signing in with a **customer account** (clients realm) grants the **client toolset**.
- Signing in with an **admin account** (admin realm) grants the **admin toolset**.

If your MCP client can't do dynamic client registration, a pre-registered public client `proxcloud-mcp` exists in both realms (PKCE required).

### 2. API key (non-interactive)

Create an HMAC key pair under **System → API keys** in the admin console (the secret is shown once). Then point the MCP client at the server with a static bearer header that joins the pair with a dot:

```json
{
  "mcpServers": {
    "proxcloud-admin": {
      "type": "http",
      "url": "https://<api-host>/mcp",
      "headers": {
        "Authorization": "Bearer pk<32hex>.sk<40hex>"
      }
    }
  }
}
```

API-key principals always get the **admin toolset**. Treat the pair like any admin credential: it's checked with a constant-time compare, but it rides in the header — use HTTPS only, and rotate it via System → API keys.

## The admin toolset

The core directory tools wrap the [Admin API](/docs/reference/overview):

- Reads: `list_users`, `get_user`, `list_organizations`, `get_organization`, `list_org_members`, `list_projects`, `get_project`, `list_billing_profiles`, `get_billing_profile`, `list_bills`, `get_bill`, `list_account_credits`, `get_account_credit`, `list_service_providers`, `get_service_provider`.
- Writes: `create_user`, `delete_user`, `create_organization`, `create_organization_billing_profile`, `create_project`, `provision_project`, `activate_billing_profile`, `suspend_billing_profile`, `resume_billing_profile`, `create_account_credit`, `delete_account_credit`.

On top of those, the admin toolset exposes the platform's configuration surfaces (these wrap the internal admin REST routes — same permissions and audit trail as the admin console):

| Area | Tools |
|---|---|
| Pricing | `list/get/create/update_price_plan`, `list/get/create/update/delete_price_plan_rule`, `get_price_plan_rule_usage`, `list_billing_resource_types`, `list_unpriced_flavors` |
| Billing config | `get/create/update_billing_configuration`, `list_currencies` |
| Platform config | `get/update_platform_configuration`, `set_platform_regions` |
| Cloud providers | `list/get/update_cloud_provider`, `set_provider_default_quota`, `set_provider_features`, `set_metrics_config`, `test_metrics_config`, `get_gpu_capacity`, `list_live_flavors` |
| Catalog | `list/get/create/update/delete_flavor_category`, `list/get/create/update/delete_image_category`, `list_image_category_groups`, `get/create/update/delete_image_group`, `list_live_images`, `list/get/create/update/delete/reactivate_instance_metadata_option` |
| Project ops | `set_project_quota`, `set_project_public_networks`, `sync_project`, `get_project_resource_counts`, `list_project_cloud_resources`, `update_project`, `set_project_status`, `list_project_members_admin` |
| Transactions | `list_account_credit_transactions`, `list_collect_transactions`, `list_billing_profile_transactions`, `refund_transaction`, `approve/reject_bank_transfer` |
| Catalog & campaigns | savings plans, promotion codes, price-adjustment rules, taxes, custom menu, message templates (list/create/update/delete each) |
| Integrations | `list/create/update/delete_integration` (secrets are write-only, never returned) |
| Observability | `get_admin_stats`, `search_audit_log` |

Update tools that replace a stored document wholesale (`update_platform_configuration`, `update_billing_configuration`, `set_provider_default_quota`, `set_provider_features`) say so in their descriptions — read first, merge, write back.

Mutations by API-key principals are recorded in the audit log with the key id as the actor.

## Good to know

- Responses are the raw JSON envelopes from the REST API; failed calls surface as tool errors carrying the HTTP status and body.
- Keyset pagination behaves the same way: pass the previous page's `next_marker` back as `marker`.
- The endpoint lives on the API host/ingress at path `/mcp` — there's no extra service or port to run.
