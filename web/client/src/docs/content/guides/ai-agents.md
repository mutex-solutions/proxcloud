# AI Agent Access (MCP)

ProxCloud ships a built-in [Model Context Protocol](https://modelcontextprotocol.io) server, letting AI assistants such as Claude Code or Claude Desktop operate your cloud on your behalf — listing servers, checking costs, rebooting an instance — under your own account and permissions.

- **Endpoint:** `https://<api-host>/mcp`
- **Scope:** precisely what you can do in the portal, and nothing beyond it. Every tool call travels the same API, project-membership checks, and audit log as the web console.

## Connecting your account

Register the server, and sign in when your MCP client pops open a browser:

```bash
claude mcp add --transport http proxcloud https://<api-host>/mcp
```

The first call comes back with an authentication challenge. Your MCP client discovers the ProxCloud identity provider, opens the same login page you already know, and finishes by redirecting back to the client — standard OAuth with PKCE. Sign in with your usual portal credentials.

A client that cannot sign in through a browser can send a personal access token from your account page instead, as the header `Authorization: Bearer <pk>.<sk>`. It acts as you, like a login; a read-only token is refused on `/mcp`, because tools can write.

## What the tools can do

**Read:** your profile; projects, organizations, and members; servers, volumes, networks, floating IPs, load balancers, security groups, images; live flavors; project costs and balance; billing profiles and bills; sandboxes and their logs.

**Act:** server power actions (soft/hard reboot, start, stop); create and delete volumes (with a `serverId` the volume is a data disk of that server, without one a standalone volume you attach later; a volume must be detached before it can be deleted); invite a teammate to a project; create a sandbox, run a command in it, change its timeout and delete it, where the project has sandboxes.

A couple of prompts to try once you're connected:

> "List my servers in demo-project and reboot the one named web-01."
>
> "How much has this project cost so far this month, and what's driving it?"

## Things to keep in mind

- Destructive tools (deleting a volume) act on whatever resource id you hand them — the assistant shows you the id before it acts, so read it.
- Sessions track your login token's lifetime; the client refreshes or re-prompts on its own.
- Administrators get a separate admin toolset, documented in the admin materials.
