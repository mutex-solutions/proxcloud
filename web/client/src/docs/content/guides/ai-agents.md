# AI Agent Access (MCP)

ProxCloud ships a built-in [Model Context Protocol](https://modelcontextprotocol.io) server, letting AI assistants such as Claude Code or Claude Desktop operate your cloud on your behalf — listing servers, checking costs, rebooting an instance — under your own account and permissions.

- **Endpoint:** `https://<api-host>/mcp`
- **Scope:** precisely what you can do in the portal, and nothing beyond it. Every tool call travels the same API, project-membership checks, quotas and audit log as the web console.

## Connecting your account

Register the server, and sign in when your MCP client pops open a browser:

```bash
claude mcp add --transport http proxcloud https://<api-host>/mcp
```

The first call comes back with an authentication challenge. Your MCP client discovers the ProxCloud identity provider, opens the same login page you already know, and finishes by redirecting back to the client — standard OAuth with PKCE. Sign in with your usual portal credentials. If your MCP client cannot register itself with the identity provider, ask your provider for an MCP client id.

### With a personal access token

For an agent that runs without a browser, create a personal access token on your account page and send it as a header:

```json
{
  "mcpServers": {
    "proxcloud": {
      "type": "http",
      "url": "https://<api-host>/mcp",
      "headers": { "Authorization": "Bearer pk<32hex>.sk<40hex>" }
    }
  }
}
```

The token acts as you. It needs the **full** scope: a read-only token is refused, because the tools can make changes.

## What the tools can do

**Read:** your profile; projects, their services, organizations and members; servers, volumes, networks, floating IPs, security groups, images, load balancers; flavors and the public image catalog; Kubernetes clusters, databases, DNS zones and records, sandboxes and sandbox templates; backups and snapshots; project costs and balance; billing profiles and bills.

**Act:**

- Servers: create a server, power actions (soft/hard reboot, start, stop), back up, restore, snapshot and roll back, turn the Protected add-on on or off.
- Volumes: create one on a server or as a standalone volume; delete a detached volume (an attached one must be detached first).
- Kubernetes: create a cluster, change its node groups, upgrade it, fetch its kubeconfig.
- Databases: create one, take and list backups.
- DNS: create, replace and delete records.
- Sandboxes: start one, run commands in it, read its output, get its access details, extend its timeout, delete it.
- Invite a teammate to a project.

A couple of prompts to try once you're connected:

> "List my servers in demo-project and reboot the one named web-01."
>
> "How much has this project cost so far this month, and what's driving it?"
>
> "Start a sandbox, run `python3 --version` in it, then delete it."

## Things to keep in mind

- Destructive tools (deleting, restoring, rolling back, replacing DNS records) act on whatever resource id you hand them — the assistant shows you the id before it acts, so read it.
- A kubeconfig or a sandbox's access tokens fetched by the assistant end up in your conversation with it. Treat that conversation like the credentials it holds.
- Creates are billed and count against your project's quota exactly as in the portal.
- Sessions track your login token's lifetime; the client refreshes or re-prompts on its own.
- Administrators get a separate admin toolset, documented in the admin materials.
