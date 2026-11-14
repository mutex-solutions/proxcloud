# E2B compatibility

ProxCloud sandboxes speak the [E2B](https://e2b.dev) API, so the stock E2B SDKs work against them. Set two variables
and use the SDK as usual:

```bash
export E2B_API_URL=https://api.example.org/e2b   # the portal's API host + /e2b (console: Sandboxes > API keys)
export E2B_API_KEY=e2b_…                          # a sandbox API key of your project
```

```python
from e2b import Sandbox

sbx = Sandbox.create("base")                      # a template listed by GET /templates
print(sbx.commands.run("echo hi").stdout)
sbx.files.write("/home/user/a.txt", "hello")
sbx.kill()
```

`E2B_DOMAIN` is not needed: the create answer carries the sandbox domain, and the SDK reaches the sandbox at
`https://<port>-<sandboxID>.<domain>` with it. This page lists, endpoint by endpoint, what works, what behaves
differently and what does not, with the reason. Reference: E2B's `spec/openapi.yml` and envd at e2b-dev/infra
`e85b9f5` (envd 0.10.2). Design and lab measurements: [managed-sandbox.md §5](managed-sandbox.md#5-the-e2b-compatible-api-sb5).

## Current saved-lifecycle availability

Managed manual/automatic pause, resume, paused CONNECT/wake, fork and new
AutoPause/AutoResume promises are **held with a fixed 403 before lifecycle effects**.
The provider-wide capability is false without an override; copied CREATE annotations
and dormant SnapshotProof correlations are not restore authority. Historical copies
are retained on refusal, but the original source can still expire at its existing
shutdownTime. No hidden lease renewal, TTL extension or retry clears this hold.

Named filesystem snapshot capture/export, pinned CREATE from a ready named snapshot,
ordinary CREATE and existing running CONNECT/GET/exec/timeout remain supported where
offered. Explicit authorized discard keeps its uncertainty/active-lease fences.
See [saved-runtime admission](saved-runtime-admission.md). Earlier SB7 lifecycle
algorithm details below are historical and do not promise reachable managed restore.

## How it fits together

| E2B piece | ProxCloud |
|---|---|
| `https://api.<domain>` (REST, `X-API-Key`) | `proxcloud-api` `/e2b/*` on the portal's API host (`E2B_API_URL`) |
| `https://<port>-<sandboxID>.<domain>` (envd on 49983, user ports) | the entry appliance and `sandbox-proxy` on the sandbox cluster, TLS with the sandbox domain's wildcard certificate (managed-sandbox.md §4) |
| Firecracker microVM | gVisor pod (`runsc`, systrap) on the platform's sandbox cluster |
| envd | the same envd, built unmodified from E2B's source, started by `sandbox-init` as PID 1 |
| Team API key | **sandbox API key**: bound to one project, acts as the member who created it |

Billing: the native sandbox price rules, per minute from the moment the sandbox is ready until it is killed or times
out, rounded up to the minute with a one-minute minimum (E2B bills per second). The project's sandbox quota, the
suspension of a project or its billing profile and the provider's release gates apply exactly as on the native API.

## Bandwidth and unattended compute (SB10)

Every new sandbox, including SDK-created ones, has a provider default egress cap of **100 Mbps**. An operator-curated
built-in template may override it; tenants cannot disable or raise it through `envVars`, metadata, network rules or
custom templates. It covers TCP, UDP, same-node traffic and responses through sandbox-proxy (not ingress downloads).
The host configures the real pod network namespace before any customer code starts; gVisor root cannot remove it.
The native console/service limits show the configured effective cap. Existing pods keep their old cap until recreated;
new ordinary starts use the current provider/trusted-template cap. Managed resume/fork are currently held, not another
way to recreate a pod. Short bursts are possible (at least 64 KiB token bucket).

A minutely heuristic alerts the operator when a sandbox stays near its CPU limit without observed exec or traffic
activity for 15 minutes by default. This is **not proof of mining**: a legitimate long unattended job may match.
Alerts do not stop sandboxes. Automatic pause/kill configuration is **rejected for envd/E2B providers** (legacy
incompatible documents still refuse actions safely): a silent active SDK streaming exec has no trustworthy completion signal, so node bytes alone cannot prove
exec inactivity. The operator reviews alerts and acts manually. Native-only providers can explicitly opt into destructive kill under the native evidence checks. Automatic filesystem
pause remains blocked by the provider-wide saved-lifecycle hold, even when the stored policy requests it.
SDK exec/file/proxy traffic counts as node-observed network activity. Periodic traffic or native exec can evade the rule;
it is a triage signal, not a guarantee. Details: [operator controls](managed-sandbox.md#10-abuse-controls-sb10).

## API keys

Create keys in the console (Sandboxes > API keys) or with the client API:
`POST /api/v1/project/{id}/sandbox-api-keys {name?}` returns the key once; `GET` lists them (hint only); `DELETE
…/{keyId}` revokes it at once. A key stops working when its creator loses the right to manage cloud resources in the
project. Only the key's SHA-256 is stored. The key goes in `X-API-Key`, as the SDKs send it; `Authorization: Bearer
e2b_…` works too. A personal API token (`pk.sk`) is refused: it is not tied to one project. Deleting the project deletes
its keys.

Revoking a key (or removing its creator) does not touch sandboxes already started: each keeps its envd access token,
so a client that holds a sandbox's envd token keeps reaching it (commands, files) until the sandbox ends at its
timeout, as on E2B. Kill those sandboxes to cut that access at once.

## REST API (`/e2b`)

Status: **yes** = same request and response as E2B; **partial** = works, with the noted difference; **no** = not
served, answers 404 with E2B's error body (or 400 for a refused field).

### Sandboxes

| Endpoint | Status | Notes |
|---|---|---|
| `GET /health` | yes | |
| `POST /sandboxes`, `POST /v2/sandboxes` | partial | `templateID` (a built-in template), `timeout` (1 s up to the provider's maximum, 24 h by default; 300 s when not sent), `metadata`, `envVars`, `network`, `allow_internet_access`. Returns once the sandbox is ready (envd up, the template's ready command passed); all templates use phased startup (default total/scheduling/pull/runtime 900/120/720/600 s, hard total 1800 s), bounded by caller cancellation. Runtime TTL starts once at the first committed Ready, not at create. A finite provisional startup+runtime deadline protects a crashed request; late cache/access reads cannot return usable success. Failure removes the sandbox and answers 503. `secure` is ignored (envd always requires its token). `autoPause`/`autoResume` promises are currently held with 403; invalid memory/configuration requests retain 400 precedence. `volumeMounts` → 400; `mcp` must be an object (the SDK starts the gateway in an `mcp-gateway` sandbox; the API never stores or logs it). `iam` and `Idempotency-Key` are ignored. ProxCloud extensions: `cpuCount`, `memoryMB` (rounded up to whole GiB), `gpu {model, count}` (no GPU nodes before SB9) |
| `GET /sandboxes`, `GET /v2/sandboxes` | partial | `metadata` and `state` filters; running and paused sandboxes; one page holds everything, so `nextToken` is never set |
| `GET /sandboxes/{id}` | yes | `diskSizeMB` is 0 |
| `DELETE /sandboxes/{id}` | yes | |
| `POST /sandboxes/{id}/timeout` | yes | |
| `POST /sandboxes/{id}/refreshes` | yes | |
| `POST /sandboxes/{id}/connect`, `/v2/…/connect` | partial | Returns the sandbox and extends its timeout; paused CONNECT is currently held with 403; running CONNECT remains supported |
| `PUT /sandboxes/{id}/network` | partial | `allowOut` / `denyOut` / `dnsServers`. `egressProxy`, `rules`, `maskRequestHost`, `httpsPorts` → 400. `allowPublicTraffic` only at create |
| `GET /sandboxes/{id}/logs`, `/v2/…/logs` | partial | The sandbox's stdout/stderr (envd and the template's start command), newest 1000 lines; no `start`/`limit` or cursor |
| `GET /sandboxes/{id}/metrics`, `GET /sandboxes/metrics` | partial | Always empty |
| `POST /sandboxes/{id}/pause`, `/resume`, `/snapshots`, `/fork`, `GET /snapshots` | partial | Named filesystem snapshots remain templates of the project, billed per GB-hour until deleted; named-snapshot CREATE may answer 503 while assembly is pending. Managed pause/resume/fork are currently held with 403; invalid memory/count/timeout checks still precede admission |

### Templates

| Endpoint | Status | Notes |
|---|---|---|
| `GET /templates`, `GET /templates/{id}`, `GET /templates/aliases/{alias}` | yes | The project's own templates (`public: false`), then the location's built-in ones (`public: true`) |
| `POST /v3/templates` | yes | `name` (or `alias`, `name:tag`), `tags`, `cpuCount`, `memoryMB` (whole GiB), independent `buildCpu`, `buildMemoryGiB`, `buildDiskGiB`; at most 20 templates per project; a built-in name → 409. 503 where the location has no template registry or build bucket |
| `GET /templates/{id}/files/{hash}` | yes | The upload URL points at this API (`PUT …/upload?token=…`), not at a bucket; up to 2 GiB per context, 10 GiB per project (507 above) |
| `POST /v2/templates/{id}/builds/{buildID}` | partial | Steps `RUN`, `COPY`, `ENV`, `WORKDIR`, `USER`; `fromImage`, `fromTemplate`, `fromImageRegistry` types `registry`, `gcp` and `aws` (explicit caller keys exchanged for a region-matching ECR token); `startCmd`, `readyCmd`. No layer cache between builds, 30 minutes per build, provider-configured concurrency (default 2) per project; billed using the independent builder size while it runs; passes the sandbox create's checks (409, 403, 503) |
| `GET /templates/{id}/builds/{buildID}/status` | yes | `logsOffset`; stable append-only paginated stage-prefix logs, `nextLogsOffset`, phase/exit/OOM/resource diagnostics; explicit bounded truncation and unavailable-output markers survive worker cleanup |
| `DELETE /templates/{id}`, `GET /templates/{id}/tags`, `POST /templates/tags`, `DELETE /templates/tags` | yes | Alias-path deletion removes only that alias. Canonical/id deletion removes a record only when its operations permit safe teardown. Both physical registry deletion paths remain HOLD; no image pruning is enabled by this port |
| `POST /templates/import` | extension | Immutable digest verification or trusted remote copy; idempotent source/runtime settings, project aliases, sanitized provenance and independent runtime size. See policy below |
| `POST/DELETE /templates/{id}/aliases` | extension | Alternate names, not version tags; primary name cannot be removed |
| `POST/GET /templates/{id}/prepare` | extension | Finite eligible-node N/M image-cache coverage, default-off; not workload readiness |
| `/v2/templates`, `PATCH /templates/{id}`, v1 builds, `/unstable/builds*` | no | Older build APIs; the SDKs from 2.x use v3 |

### Not served

Volumes (`/volumes`, `/volumecontent/*`), secrets (`/secrets`), events and webhooks (`/events/*`), team endpoints
(`/teams/*`), `/api-keys`, `/default-templates`, everything under `/admin`, `/nodes`, `/clusters`, `/unstable` and the
dashboard APIs. They answer 404 with E2B's error body.

## Inside the sandbox (`<port>-<id>.<domain>`)

| Feature | Status | Notes |
|---|---|---|
| Commands (`process.Process`: start, connect, list, send input, signal, PTY) | yes | envd itself; output streams through the proxy |
| Files (`filesystem.Filesystem`, `/files` upload and download, watch) | yes | envd itself |
| `get_host(port)` / preview URLs | yes | Any port the sandbox listens on, HTTP and WebSocket; ports bound to localhost too (sandbox-init relays them) |
| `trafficAccessToken` (`network.allowPublicTraffic: false`) | yes | Header `e2b-traffic-access-token` on every application port; port 49983 is exempt only when the pod actually runs envd (which checks its own token). Native private services require the traffic token there too. |
| envd control routes (`/init`, `/freeze`, …) | blocked | Refused by the proxy, as in E2B |
| Default user | yes | `user` with home `/home/user`; created at boot if the image has none |
| Environment | partial | Image `ENV` + `envVars` reach every command; `HOME`/`USER` are per user |

## Built-in templates

| E2B template | ProxCloud | Notes |
|---|---|---|
| `base` (SDK default) | yes | E2B's base image plus user `user` with passwordless sudo, `/code` |
| `code-interpreter-v1` (`e2b-code-interpreter`) | yes | Python, JavaScript and Bash kernels; R and Java are not installed. Its Jupyter readiness uses the configured phased startup budget |
| `mcp-gateway` (`Sandbox.create(mcp={…})`) | yes, when the provider offers it | The base plus `cmd/mcp-gateway` and its preinstalled servers; 47 servers in `cmd/mcp-gateway/catalog.json` (remote ones and package ones baked in, no Docker image servers). Each server's keys come from the tenant's `mcp` config; see [managed-sandbox.md §9](managed-sandbox.md#9-mcp-gateway-sb8) |
| `desktop`, `codex`, `opencode`, `claude`, `amp`, `gemini`, other agent templates | no | Images in `deploy/templates/`, offered once the template workflow pinned them (SB6a) |

## What does not work, and why

1. **Managed pause/resume/fork** are currently unavailable, not an automatically
   retryable operation. New AutoPause/AutoResume create requests are refused before
   CREATE owner/pin/cluster effects. An existing historical paused record is retained
   on refusal; its source deadline is not rearmed. Memory snapshots remain unsupported.
2. **Named filesystem snapshots** remain supported separately. They do not preserve
   processes, RAM, connections or whiteouts. A ready named image is started through
   the already-pinned ordinary CREATE protocol, not the held saved-runtime path.
3. **Slow named-snapshot CREATE** waits at most SnapshotRequestWait and can answer
   503 while assembly continues. Retrying that named CREATE does not recapture; fork
   itself is held403 and creates no retry snapshot or children.
4. **Recovery prerequisites** are still incomplete: trusted source/capture/provider
   lineage and enforceable exclusion of old issued Apply/controller recreation. A
   missing CR, lease age or new operation/head CAS does not supply either proof.
5. **Custom templates**: the v3 build API works (`Template()` builder, `from_dockerfile`,
   `e2b template build`, tags and delete). ECR uses `type: "aws"` with explicit
   `awsAccessKeyId`, `awsSecretAccessKey`, `awsRegion`; the image must be a matching
   `<account>.dkr.ecr.<region>.amazonaws.com` host. Only the exchanged token reaches the
   worker; there is no ambient/default AWS credential fallback. The existing `registry`
   type with username `AWS` and an already obtained ECR password also works.
   Successful/tagged build history is retained; moving a tag is not artifact deletion.

6. **Templates beyond `base`, `code-interpreter-v1` and `mcp-gateway`** (`desktop`, agent CLIs): no images before the
   template workflow (SB6a) pinned them. Build your own from `base`.
7. **`Sandbox.create(mcp=…)`**: only the servers of the catalog (`mcp-gateway list` in the sandbox) plus
   `github/<owner>/<repo>` servers with a `run_cmd`; any other name fails the create ("not available on ProxCloud").
   E2B's servers that run from a Docker image and have no remote endpoint are not offered (no Docker inside gVisor).
   A server's keys come only from the tenant's config; a missing required field fails the create. The gateway restarts
   with the sandbox (`mcp-gateway boot`), ordinary fresh CREATE initializes the same offered gateway tools; managed resume is currently held.
8. **Volumes** (`volumeMounts`, `/volumes`): 400 / 404. A sandbox has ephemeral storage only, by design: the sandbox cluster has no CSI
   token and no NAS dataset, so an escape from gVisor finds no storage credential.
9. **Network `egressProxy`, `rules`, `maskRequestHost`, `httpsPorts`**: 400. The egress policy is Cilium (CIDR and
   domain allow and deny lists); there is no egress proxy or request rewriting, and the proxy forwards plain HTTP to
   the pod.
10. **`allowPublicTraffic` after create**: 400. The traffic token is written into the pod at create.
11. **Private ranges**: RFC1918, CGNAT and link-local destinations are unreachable whatever `allowOut` says, including
    domains that resolve to them (the guardrail protects the platform's and other tenants' networks).
12. **Metrics**: always empty; no per-sandbox metrics are collected.
13. **Logs filters** (`start`, `limit`, cursor): not supported; the newest 1000 lines.
14. **Slow starts**: ordinary create and native create operations use the
    provider's phased startup policy, with one cancellation/deadline envelope including cache
    and final access reads. Set the SDK's request timeout long enough for the configured
    budget (for the default total, e.g. `request_timeout=930`, `retries=0`). Runtime TTL
    starts at first committed Ready and never shifts on readiness flaps/restarts. Failed
    startup cleanup is bounded to 30 s. An uncertain create must not be blindly retried
    (`Idempotency-Key` remains ignored). An uncertain resume retains its lease/snapshot;
    lease age or a missing GET is not proof allowing a duplicate new create.

15. **Secrets, events, webhooks, teams, `/api-keys`, admin and dashboard APIs**: not served. Keys are managed in the
    console or the client API; there are no teams (a key belongs to a project).
16. **`secure: false`**: ignored; envd always requires its access token.
17. **`iam`, `Idempotency-Key`**: ignored. A retried create can start a second sandbox.
18. **GPUs**: none before SB9 (no GPU nodes on the sandbox cluster).
19. **Kernel**: gVisor implements Linux in user space. Almost everything runs; some syscalls, `/proc` entries and kernel
    features (io_uring, for example) are missing or partial. `uname -r` reports `4.19.0-gvisor`. Docker in a sandbox does
    not work.

## Behaviour that differs

- **Start time**: a few seconds once the image is on the node (E2B resumes a snapshot in about 150 ms); the first start
  of a large image waits for its pull.
- **IDs**: sandbox IDs look like `sbx-1a2b3c4d5e6f7a8b`; `clientID` is always `proxcloud`.
- **Limits**: the provider's per-sandbox limits and the project's sandbox quota (count, vCPU, RAM, GPU; 429 when
  exceeded). There are no team-level limits.
- **Suspension**: a suspended billing profile deletes the project's running sandboxes and refuses new ones (403); a
  DISABLED project refuses new ones (409); a DISABLED provider or a release gate that does not hold refuses them (503).
  Reads and kill stay open.

## OpenAPI document

`GET /e2b/openapi.json` (no key) is E2B's own OpenAPI document (e2b-dev/runtime `spec/openapi.yml` at e85b9f5,
Apache-2.0) cut down to the operations above, converted to OpenAPI 3.1, with ProxCloud's differences in each
operation's description and the create extensions in `NewSandbox`. Regenerate it with
`internal/platform/project/e2b_openapi_gen.py`; `TestE2BOpenAPIParity` fails when the router and the document disagree.

## Errors

E2B's shape: `{"code": <http status>, "message": "…"}`.

| HTTP | When |
|---|---|
| 400 | invalid input or a refused field (`volumeMounts`, invalid memory/configuration, `mcp` that is not an object, `network.rules`, …); those checks may precede capability admission |
| 401 | missing, bad or revoked key |
| 403 | the key's creator can no longer manage sandboxes in the project; billing profile suspended; sandboxes not enabled for the project; managed saved-lifecycle/AutoPause/AutoResume capability hold |
| 404 | unknown sandbox or template, a sandbox of another project, an endpoint that is not served |
| 409 | the project is DISABLED or being deleted |
| 429 | the project's sandbox quota |
| 503 | the provider is DISABLED, a release gate does not hold, the location does not run envd, the cluster is unreachable, or the sandbox did not start in time |

On sandbox URLs (`<port>-<id>.<domain>`), the proxy answers 502 for an unknown sandbox and 403 when the traffic access
token is missing or wrong.

## Compatibility suite

`test/e2b-compat/compat.py` runs the stock SDKs end to end against a location (commands, streaming, exit codes, files,
preview URL, metadata, connect, egress rules, the metadata address, a second project's key, the code interpreter):
`E2B_API_URL`, `E2B_API_KEY`, optionally `E2B_OTHER_API_KEY` (a key of another project) and `SSL_CERT_FILE` for a
private CA. `test/e2b-compat/pause.py` records the older lifecycle acceptance drill; its historical restore-success
expectations are **not** evidence that current managed pause/resume/fork or AutoPause/AutoResume can be enabled.
Current admission tests must expect the fixed 403 and unchanged retained state, while testing named snapshot
capture/export and ordinary CREATE separately. Do not use the debug auto-pause trigger or retries to bypass the hold;
see [saved-runtime admission](saved-runtime-admission.md) and
[historical SB7 notes](managed-sandbox.md#8-pause-resume-snapshots-and-fork-sb7).

## Immutable digest import and session console

`POST /templates/import` accepts `source`, `name`, optional `aliases`/`tags`, runtime
`cpuCount`/`memoryMB`, `user`, `workdir`, `startCmd`, `readyCmd`, and optional caller-owned
`registry {username,password}`. It returns `202 {templateID,buildID,status,aliases,reused}`;
poll the existing build-status endpoint. Repeating exact source/runtime settings adds
names to the same project record, without another worker. Aliases are template names;
tags remain pointers to build versions. Imported ENV is preserved, but sandbox-init
replaces the image ENTRYPOINT/CMD. No customer image executes during import.

Operator policy is `config.sandbox.import`:

```json
{
  "sourceRepositories": {
    "https://source.example.org:5443": ["approved/app", "projects/{project}/app"]
  },
  "allowAnonymous": false
}
```

Origins are canonical HTTPS origins with optional port and no userinfo/path/query.
Repository entries are **exact**, not prefixes or wildcards; only literal `{project}`
substitution is supported. The empty policy admits only the configured registry's
`t/<current-project>/<20-lowercase-hex>@sha256:<digest>` namespace, verified using that
project's pull identity. Foreign managed project paths remain forbidden even if listed.
A same-host customer repository can be explicitly approved without granting access to
another managed namespace. External repositories require caller credentials. Only an
explicit operator `allowAnonymous: true` permits an empty anonymous `auths` document;
there is never source-credential fallback to a platform robot. The trusted worker
independently reenforces fixed config-derived policy, repository credential scoping,
redirect/token-realm restrictions and immutable result identity.

The worker image is read from `sandbox-system/sandbox-import-worker`, key `image`, and
must be `<configured-host>/library/proxcloud-sandbox-import@sha256:<digest>`. Capabilities
remain disabled if this trusted worker, valid policy or durable registry protection is
unavailable. Warming also requires the trusted pull-only runtime capability and eligible
nodes; completed pull pods alone do not establish current cache coverage.

The session console uses `/api/v1/project/{id}/sandbox-templates`: list, capabilities,
import, alias add/remove, prepare, build logs/status and delete. `POST /{templateId}/builds`
is **new-template only** and accepts 1–100 bounded RUN steps on that existing project
base, runtime size and independent builder budgets. It does not accept COPY uploads,
ENV, arbitrary base image/registry fields, or caller project/identity overrides. Every
console route, including reads, intentionally requires session cloud-resource management
permission. No browser API key or API-key administration prerequisite is added. Cancel
in the build dialog closes the dialog; no public build-cancel API is provided.

Unknown-template errors list a bounded ready-only current-project catalog and link to
the configured customer console's `/docs/api/e2b` page, never an untrusted Host header.

**Safety scope:** BUILD and IMPORT retain distinct one-shot durable operation proofs.
An ambiguous apply/cancellation, timeout or missing pod retains UNKNOWN and cannot be
cleared by cleanup. Both physical registry manifest-delete paths remain unconditional
HOLD until all writer/adoption coverage is proven. Snapshot producers and other existing
writers are not claimed as universally integrated by import support. See the
[C4 API contract](architecture/c4/sandbox-api-port.md) for the inherited provider-wide
rotation SQL callback compatibility exception and exact handoff sequence.
