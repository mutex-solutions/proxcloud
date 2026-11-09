# MCP Server

ProxCloud bundles a [Model Context Protocol](https://modelcontextprotocol.io) server, so AI agents — Claude Code, Claude Desktop, or any MCP-capable client — can drive the platform through curated tools rather than raw HTTP.

- **Endpoint:** `https://<api-host>/mcp` (Streamable HTTP, stateless — safe behind multiple API replicas)
- **Toolsets:** the tools you're offered depend on who you are. Operators (an admin-realm sign-in or an Admin API key) get the **admin toolset**: users, organizations, projects, billing, pricing and provider configuration. Customers (a portal sign-in or a personal access token) get the **client toolset**: their own projects, cloud resources and billing.

Every tool call runs against the very same REST endpoints documented in this reference — same permissions, same validation, same suspension and quota checks, same audit trail. MCP opens no privileged side door.

## Authentication

### 1. OAuth sign-in (interactive)

Add the server with no credentials and let your MCP client run the standard OAuth flow:

```bash
claude mcp add --transport http proxcloud https://<api-host>/mcp
```

On first use the server replies `401` with an RFC 9728 resource-metadata document naming the ProxCloud identity provider (Keycloak). Your MCP client discovers it, opens a browser window and returns to a local port with an authorization code (PKCE).

The MCP client needs an OAuth client in that realm. ProxCloud does not create one for you:

- If the realm lets clients register themselves (Keycloak dynamic client registration, allowed by its client-registration policies), the MCP client does it on its own.
- Otherwise register a **public** client in the customer realm, with PKCE (S256), the standard flow, and a `http://localhost/*` / `http://127.0.0.1/*` redirect URI, and give its client id to your MCP client.

What the sign-in grants:

- A **customer account** (the customer realm) gets the **client toolset**, whatever client the token was issued to: tool calls run as that user, under the same membership, role and suspension checks as the portal. Once any organization has per-organization SSO set up, the client must also request the `proxcloud-sso` scope, or the sign-in is refused.
- An **operator account** (the admin realm) is offered the admin toolset, but the admin routes accept operator tokens only from the admin console's own client (`auth.admin.clientId`), and the Admin API only from `auth.adminApi.clientId`. A token from any other client gets `403` on every admin tool. Operators should use an API key (below).
- When the customer and operator realms are the same issuer (a development setup), every sign-in is routed to the admin toolset.

### 2. Keys (non-interactive)

Both kinds of key are a pair joined with a dot, sent as a static bearer header:

```json
{
  "mcpServers": {
    "proxcloud": {
      "type": "http",
      "url": "https://<api-host>/mcp",
      "headers": {
        "Authorization": "Bearer pk<32hex>.sk<40hex>"
      }
    }
  }
}
```

- **Admin API key → admin toolset.** Create one under **System → API keys** in the admin console (the secret is shown once), or from a shell in the API pod: `proxcloud-api admin-key create -description "mcp agent"` prints `{id, secretKey}` once (it writes to PostgreSQL through `PROXCLOUD_DB_URL`, so it needs no admin login). Join them as `<id>.<secretKey>`. Mutations made with the key are audited with the key id as the actor. Treat the pair like any admin credential: HTTPS only, and delete it under System → API keys when it is no longer needed.
- **Personal access token → client toolset.** A customer mints one on their account page; tool calls act as that user. A token with **read** scope is refused on `/mcp` (`401`), because MCP tools can write; use a full-scope token.

## The client toolset

| Area | Tools |
|---|---|
| Account and projects | `get_current_user`, `list_projects`, `get_project`, `list_project_services`, `list_organizations`, `list_org_members`, `invite_user_to_project` |
| Billing | `get_project_billing`, `get_project_cost`, `list_billing_profiles`, `list_bills` |
| Resources | `list_servers`, `list_volumes`, `list_networks`, `list_floating_ips`, `list_security_groups`, `list_images`, `list_load_balancers`, `get_cloud_resource` |
| Servers | `list_flavors`, `list_public_images`, `create_server`, `server_action` (reboot, start, stop) |
| Volumes | `create_volume` (on a server, or standalone), `delete_volume` (a detached volume) |
| Backups and snapshots | `list_backups`, `create_backup` (server or database), `restore_server_backup`, `delete_server_backup`, `list_server_snapshots`, `create_server_snapshot`, `rollback_server_snapshot`, `delete_server_snapshot` |
| Protection | `enable_server_protection`, `disable_server_protection` |
| Kubernetes | `list_kubernetes_clusters`, `create_kubernetes_cluster`, `get_kubeconfig`, `set_kubernetes_node_groups`, `upgrade_kubernetes_cluster` |
| Databases | `list_databases`, `create_database` (backups through `list_backups` / `create_backup`) |
| DNS | `list_dns_zones`, `create_dns_ownership_challenge`, `get_dns_ownership`, `verify_dns_ownership`, `cancel_dns_ownership`, `create_dns_zone`, `list_dns_records`, `set_dns_record`, `delete_dns_record` |
| Sandboxes | `list_sandboxes`, `list_sandbox_templates`, `create_sandbox`, `exec_in_sandbox`, `get_sandbox_logs`, `get_sandbox_access`, `set_sandbox_timeout`, `delete_sandbox` |

`list_project_services` is where the create tools get their inputs: the service id that `create_kubernetes_cluster` and `create_database` must name, and each service's catalog (Kubernetes versions and flavors, database engines, sandbox templates and limits).

For a new DNS zone, run `create_dns_ownership_challenge` with its attached provider and domain, publish the returned exact TXT value at the **current DNS host**, then `verify_dns_ownership` and `create_dns_zone` with that challenge id within10minutes. The value is shown once, with only a scoped hash stored. Pending challenges expire in30minutes and reserve nothing. No tenant DNS server, CNAME shortcut, raw token or `verified:true` bypasses the gate; native HTTP and MCP share it. Wait10seconds between checks and60seconds between zone-create attempts per project, allowing DNS caches to propagate. On an uncertain create, check `list_dns_zones` before starting another challenge. Operator `ownershipResolvers` overrides are trust configuration, not tool inputs or production proof from a private lab resolver.

Two tools return credentials into the agent's conversation: `get_kubeconfig` (a cluster-admin kubeconfig) and `get_sandbox_access` (the sandbox's access tokens). Sandbox API keys, deleting a project and the Kubernetes maintenance actions stay in the portal.

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
| Operator checks | `test_proxmox_connection`, `test_object_store`, `get_sandbox_release_gates`, `list_billing_profile_suspensions` (all read-only) |
| Catalog | `list/get/create/update/delete_flavor_category`, `list/get/create/update/delete_image_category`, `list_image_category_groups`, `get/create/update/delete_image_group`, `list_live_images`, `list/get/create/update/delete/reactivate_instance_metadata_option` |
| Project ops | `set_project_quota`, `set_project_public_networks`, `sync_project`, `get_project_resource_counts`, `list_project_cloud_resources`, `update_project`, `set_project_status`, `list_project_members_admin` |
| Transactions | `list_account_credit_transactions`, `list_collect_transactions`, `list_billing_profile_transactions`, `refund_transaction`, `approve/reject_bank_transfer`, `record_bill_payment` |
| Campaigns and catalog | savings plans, promotion codes, price-adjustment rules, taxes, custom menu (list/create/update/delete each); message templates (list/get/update) |
| Integrations | `list/create/update/delete_integration` (secrets are write-only, never returned) |
| Observability | `get_admin_stats`, `search_audit_log` |

Update tools that replace a stored document wholesale (`update_platform_configuration`, `update_billing_configuration`, `set_provider_default_quota`, `set_provider_features`, `set_project_quota`) say so in their descriptions — read first, merge, write back.

## Good to know

- Responses are the raw JSON envelopes from the REST API; failed calls surface as tool errors carrying the HTTP status and body.
- Each tool carries MCP hints: reads are marked read-only, and writes that destroy or replace data (deletes, restores, rollbacks, record replacement) are marked destructive, so a client can ask before running them.
- Keyset pagination behaves the same way: pass the previous page's `next_marker` back as `marker`.
- The endpoint lives on the API host/ingress at path `/mcp` — there's no extra service or port to run.
