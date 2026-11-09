# E2B API

ProxCloud sandbox locations can offer an [E2B-compatible API](https://e2b.dev). Use the domain and network-access instructions shown under **Platform → Sandboxes → API keys**. The **Templates** and **API keys** tabs are offered only when that location supports E2B.

For console steps, see [Sandboxes](/docs/guides/sandboxes). For the browser's session-authenticated project endpoints, see [Sandbox API](/docs/api/sandboxes); those do not require an E2B API key.

## Connect an external SDK or CLI

Create a project sandbox API key on the **API keys** tab and copy it once. It acts with its creator's project permissions and stops working when revoked or when its creator loses the required access. Never embed a long-lived key in a browser application.

```bash
export E2B_DOMAIN=<domain shown on the API keys tab>
export E2B_API_KEY=<your project key>
```

```python
from e2b import Sandbox

sandbox = Sandbox.create("base")
print(sandbox.commands.run("echo hello").stdout)
sandbox.kill()
```

The SDK calls `https://api.<E2B_DOMAIN>` and reaches sandbox ports at `https://<port>-<sandbox-id>.<E2B_DOMAIN>`. Use the API route and TLS/VPN configuration given by your operator. Do not assume the console's hostname is the E2B domain or make internal registries public to reach them from CI.

The compatibility document at `https://api.<E2B_DOMAIN>/openapi.json` describes the operations the deployment serves. SDK feature support can differ from the upstream hosted service: filesystem snapshots do not capture process memory, metrics may be empty, and cloud/network/GPU limits still apply. Use a template offered by your location to request a GPU from the stock SDK.

## Templates and versions

The SDK/CLI template build flow is separate from the browser's intentionally narrow RUN-only form. The supported build flow requests a template/build with `POST /v3/templates`, uploads required contexts, then starts the build with `POST /v2/templates/{templateID}/builds/{buildID}`. Query `GET /templates/{templateID}/builds/{buildID}/status` for status and logs. Use the deployment's OpenAPI document for supported steps and upload limits.

Template **aliases** are project-scoped alternate names; **tags** select a build artifact. Use `name[:tag]` or the appropriate template ID to start a sandbox. The primary name is immutable, and records from another project must not resolve through either a name or an ID. Named snapshots may appear alongside templates; internal pause/fork bookkeeping must not appear as customer templates.

### Import a CI-built immutable image

`POST /templates/import` is a ProxCloud extension, not a stock upstream build API. It accepts the same import fields described in the [project Sandbox API](/docs/api/sandboxes#import-a-digest), authenticated with the external client's project key:

```json
{
  "source": "registry.example.com/approved/agent@sha256:<64 lowercase hexadecimal characters>",
  "name": "my-agent",
  "cpuCount": 2,
  "memoryMB": 4096
}
```

The example host is illustrative, **not a pre-approved registry**. The operator configures approved origins and exact repositories, optionally scoped with the current project. Valid OCI syntax alone is not permission. Anonymous imports are off by default. A source must be a supported linux/amd64 manifest digest; mutable tags and multi-platform indexes are not substitutes.

Private sources can accept caller-owned `registry: {username, password}` when the operator permits that mode. Credentials are for trusted pull verification only, never for customer RUN code, and must not be returned in safe template DTOs. The platform must not attach its own privileged credential to an arbitrary caller image. A first copy can transfer layers; import is not a zero-transfer guarantee.

Poll the returned `templateID` and `buildID` using build status until ready. A repeated compatible import may return `reused: true`. Imported runtime settings are immutable: choose a new template name to change them. User/workdir/start/ready overrides are bounded; the named-user format does not accept numeric UID/GID or `USER:GROUP`.

### Aliases and preparation

`POST /templates/{id}/aliases` with `{"aliases":["another-name"]}` adds names; DELETE with the same shape removes alternate names. The primary name cannot be removed. This does not reassign build tags.

Where warming is enabled, `POST /templates/{id-or-name}/prepare` requests temporary pre-pull intent, and GET reports coverage. The response includes `enabled`, `image`, `pool`, `pulled`, `total`, `ready` and optional `expiresAt`. `0/0`, expired intent and missing coverage are not readiness. Image garbage collection and new nodes can make startup cold again; coverage is no SLA.

### Build resources and diagnostics

`buildCpu`, `buildMemoryGiB` and `buildDiskGiB` are independent builder budgets within location limits. Runtime `cpuCount` and `memoryMB` do not set builder memory or build usage. The total disk budget includes compressed base data, expanded filesystem and output.

Status/log responses can include the current `step`, `buildResources`, bounded phase diagnostics and stable `logsOffset` / `nextLogsOffset` cursors. Offsets are non-negative. Termination reasons, exit codes, signals, truncated output, unavailable logs and a separately retained final failure tail explain failures after the worker is cleaned up. Older records and import-only operations need not have a detailed worker archive. There is **no worker-cancel API** in this console port; closing a dialog is not cancelling work and deleting an active template returns a retryable conflict.

## Startup and runtime boundaries

Create waits for a usable sandbox; it is not a promise that any image will start inside the SDK's short default HTTP timeout. Prepare large images or use a client timeout that covers the location's bounded startup budget (for example, Python `request_timeout=930` when the operator's budget is 900 seconds). Runtime `timeout` is separate and starts at Ready.

Files, installed packages and startup commands can survive supported filesystem snapshots or pause/resume; running processes, RAM and open connections do not. Network restrictions, access tokens, blocked internal address ranges, offered GPU models and project quotas remain enforced. Consult the location's OpenAPI and operator policy rather than assuming every upstream E2B feature or network option is supported.
