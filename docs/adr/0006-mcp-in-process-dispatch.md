# 6. Expose an MCP server that dispatches in-process through the existing API

## Status

Accepted

## Context

We want to let AI assistants operate ProxCloud on a user's behalf — list a
project's resources, read a bill, launch or pause a server — through the Model
Context Protocol (MCP). The obvious risk with any "assistant integration" is that
it becomes a second way into the system that bypasses the access controls the
REST API spent effort getting right: its own database queries, its own privilege
level, its own idea of who the caller is. That is how an integration turns into a
confused-deputy vulnerability.

We also need the MCP tools to authenticate as one of the two principals the
platform already understands (ADR-0003): a human via OIDC, or a machine via a
signed admin-API key.

## Decision

Serve MCP as a **thin adapter over the existing request pipeline**, not as a
parallel service. The implementation is `internal/platform/mcp`, mounted at
`/mcp` on the same application router and wired in `cmd/api/main.go`
(`mcpsrv.New(...)`, with the app router set as its in-process dispatch root).

- **Streamable HTTP, stateless.** The endpoint is a stateless streamable-HTTP MCP
  server (`github.com/modelcontextprotocol/go-sdk`), so it is safe behind
  multiple API replicas and needs no session store. The Helm chart's ingress
  exposes `/mcp` alongside the other public paths.
- **In-process dispatch.** Each MCP tool is a declarative row (`tools_client.go`
  and the `tools_admin*.go` files) mapping a tool name to an existing REST endpoint (method +
  path + parameters). Invoking a tool builds an HTTP request and runs it **through
  the full application router in-process** (via an `httptest` recorder), so the
  same authorization, DTO shapes, audit, and error handling apply as for a direct
  API call. An MCP tool never touches PostgreSQL or the cloud directly.
- **Dual auth, mapped to a curated toolset.** MCP requests carry the same two
  credentials as the rest of the platform:
  - an **OIDC bearer JWT**, already validated by the platform auth middleware —
    the request's realm selects the toolset (`clients` realm → client tools,
    admin realm → admin tools);
  - an **API key**, `Authorization: Bearer <pk>.<sk>`, validated against the
    `hmac_keys` table with a constant-time compare → the admin toolset.
  On dispatch, a JWT principal re-sends its own bearer; an API-key principal has
  a **SigV4 signature minted from the validated pair**, so the in-process call
  hits the same admin-API gate a real machine client would.
- **Curated tools, not blanket reflection.** The tool set is a deliberate, named
  list of safe operations; adding a tool is an explicit code change. The MCP
  surface is intentionally smaller than the REST surface.
- **Standard MCP OAuth discovery.** An unauthenticated request gets a `401` with
  a `WWW-Authenticate` challenge pointing at the RFC 9728 resource-metadata
  document (`/.well-known/oauth-protected-resource`), which names the Keycloak
  authorization server(s); MCP clients complete the OAuth flow (discovery +
  dynamic client registration + PKCE) from there.

The tool catalog is documented at `/docs/reference/mcp-server` in the admin console.

## Consequences

- Anything a caller cannot do over REST, they cannot do over MCP — authorization
  is defined once and reused, so the two surfaces cannot drift apart.
- New MCP tools are cheap to add (declare a row that wraps an existing endpoint)
  and inherit auth, policy, auditing, and error handling for free.
- Because dispatch is in-process and stateless, MCP shares the API's scaling and
  failure characteristics — no separate service to deploy, monitor, or secure,
  and it works behind multiple replicas.
- The API-key path requires the server to sign its own in-process SigV4 request
  from the validated key pair; this keeps the admin gate the single point of
  authorization but means the key's secret is used server-side to re-sign.
- The MCP surface is deliberately narrower than REST; capabilities are opt-in per
  tool, which is a feature, not a limitation.
