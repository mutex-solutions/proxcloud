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
| `POST /sandboxes`, `POST /v2/sandboxes` | partial | `templateID` (a built-in template), `timeout` (1 s up to the provider's maximum, 24 h by default; 300 s when not sent), `metadata`, `envVars`, `network`, `allow_internet_access`. Returns once the sandbox is ready (envd up, the template's ready command passed); a sandbox not ready within 50 s is removed and the call answers 503. `secure` is ignored (envd always requires its token). `autoPause`, `autoResume`, `autoPauseMemory`, `mcp`, `volumeMounts` → 400. `iam` and `Idempotency-Key` are ignored. ProxCloud extensions: `cpuCount`, `memoryMB` (rounded up to whole GiB), `gpu {model, count}` (no GPU nodes before SB9) |
| `GET /sandboxes`, `GET /v2/sandboxes` | partial | `metadata` and `state` filters; running sandboxes only (nothing is ever paused); one page holds everything, so `nextToken` is never set |
| `GET /sandboxes/{id}` | yes | `diskSizeMB` is 0 |
| `DELETE /sandboxes/{id}` | yes | |
| `POST /sandboxes/{id}/timeout` | yes | |
| `POST /sandboxes/{id}/refreshes` | yes | |
| `POST /sandboxes/{id}/connect`, `/v2/…/connect` | partial | Returns the sandbox and extends its timeout; a running sandbox only |
| `PUT /sandboxes/{id}/network` | partial | `allowOut` / `denyOut` / `dnsServers`. `egressProxy`, `rules`, `maskRequestHost`, `httpsPorts` → 400. `allowPublicTraffic` only at create |
| `GET /sandboxes/{id}/logs`, `/v2/…/logs` | partial | The sandbox's stdout/stderr (envd and the template's start command), newest 1000 lines; no `start`/`limit` or cursor |
| `GET /sandboxes/{id}/metrics`, `GET /sandboxes/metrics` | partial | Always empty |
| `POST /sandboxes/{id}/pause`, `/resume`, `/snapshots`, `/fork`, `GET /snapshots` | no | SB7 |

### Templates

| Endpoint | Status | Notes |
|---|---|---|
| `GET /templates`, `GET /templates/{id}`, `GET /templates/aliases/{alias}` | yes | The project's own templates (`public: false`), then the location's built-in ones (`public: true`) |
| `POST /v3/templates` | yes | `name` (or `alias`, `name:tag`), `tags`, `cpuCount`, `memoryMB` (whole GiB); at most 20 templates per project; a built-in name → 409. 503 where the location has no template registry or build bucket |
| `GET /templates/{id}/files/{hash}` | yes | The upload URL points at this API (`PUT …/upload?token=…`), not at a bucket; up to 2 GiB per context, 10 GiB per project (507 above) |
| `POST /v2/templates/{id}/builds/{buildID}` | partial | Steps `RUN`, `COPY`, `ENV`, `WORKDIR`, `USER`; `fromImage`, `fromTemplate`, `fromImageRegistry` types `registry` and `gcp` (`aws` → 400, see below); `startCmd`, `readyCmd`. No layer cache between builds, 30 minutes per build, 2 at once per project; billed like a sandbox of the template's size while it runs; passes the sandbox create's checks (409, 403, 503) |
| `GET /templates/{id}/builds/{buildID}/status` | yes | `logsOffset`; build logs are kaniko's, crane's and the context fetch's lines |
| `DELETE /templates/{id}`, `GET /templates/{id}/tags`, `POST /templates/tags`, `DELETE /templates/tags` | yes | A deleted template's images are pruned later (docs/managed-sandbox.md §7.4) |
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
| `trafficAccessToken` (`network.allowPublicTraffic: false`) | yes | Header `e2b-traffic-access-token` on every port but envd's |
| envd control routes (`/init`, `/freeze`, …) | blocked | Refused by the proxy, as in E2B |
| Default user | yes | `user` with home `/home/user`; created at boot if the image has none |
| Environment | partial | Image `ENV` + `envVars` reach every command; `HOME`/`USER` are per user |

## Built-in templates

| E2B template | ProxCloud | Notes |
|---|---|---|
| `base` (SDK default) | yes | E2B's base image plus user `user` with passwordless sudo, `/code` |
| `code-interpreter-v1` (`e2b-code-interpreter`) | yes | Python, JavaScript and Bash kernels; R and Java are not installed. Its Jupyter start counts towards the create's 50 s |
| `desktop`, `codex`, `opencode`, `claude`, `amp`, `gemini`, `mcp-gateway`, other agent templates | no | SB6 (built-in images through the template workflow) and SB8 (MCP gateway) |

## What does not work, and why

1. **Pause, resume, snapshots and fork** (`pause`, `resume`, `snapshots`, `fork`, `connect` of a paused sandbox,
   `state=paused`): not before SB7. A sandbox is deleted at its timeout.
2. **`autoPause` and `autoResume`**: refused with 400 rather than accepted, because without pause a sandbox that should
   pause at its timeout would be deleted and its files lost. After SB7: filesystem only.
3. **Pause with memory** (`memory: true`, `mode: "full"`, `autoPauseMemory`): never. gVisor in a pod has no microVM
   snapshot; a later resume is a cold boot (files kept, processes, memory and open connections not).
4. **Custom templates**: the v3 build API works (`Template()` builder, `from_dockerfile`, `e2b template build`, tags,
   delete). `fromImageRegistry` with `type: "aws"` (AWS keys exchanged for an ECR token) answers 400: use
   `type: "registry"` with username `AWS` and the output of `aws ecr get-login-password` as the password. No layer cache
   between builds, so a large base image is pulled again on every build. A template belongs to its project; no public
   templates.
5. **Templates beyond `base` and `code-interpreter-v1`** (`desktop`, agent CLIs, `mcp-gateway`): no images before the
   template workflow (SB6a) and the MCP gateway (SB8). Build your own from `base`.
6. **`Sandbox.create(mcp=…)`**: 400 before SB8 (the gateway runs from the `mcp-gateway` template).
7. **Volumes** (`volumeMounts`, `/volumes`): 400 / 404. A sandbox has ephemeral storage only, by design: the sandbox cluster has no CSI
   token and no NAS dataset, so an escape from gVisor finds no storage credential.
8. **Network `egressProxy`, `rules`, `maskRequestHost`, `httpsPorts`**: 400. The egress policy is Cilium (CIDR and
   domain allow and deny lists); there is no egress proxy or request rewriting, and the proxy forwards plain HTTP to
   the pod.
9. **`allowPublicTraffic` after create**: 400. The traffic token is written into the pod at create.
10. **Private ranges**: RFC1918, CGNAT and link-local destinations are unreachable whatever `allowOut` says, including
    domains that resolve to them (the guardrail protects the platform's and other tenants' networks).
11. **Metrics**: always empty; no per-sandbox metrics are collected.
12. **Logs filters** (`start`, `limit`, cursor): not supported; the newest 1000 lines.
13. **Slow starts**: create waits at most 50 s for the sandbox to be ready (the SDK's own request timeout is 60 s), so a
    template whose ready command takes longer always fails. E2B starts from a VM snapshot taken after the start command;
    here the start command runs at every boot.
14. **Secrets, events, webhooks, teams, `/api-keys`, admin and dashboard APIs**: not served. Keys are managed in the
    console or the client API; there are no teams (a key belongs to a project).
15. **`secure: false`**: ignored; envd always requires its access token.
16. **`iam`, `Idempotency-Key`**: ignored. A retried create can start a second sandbox.
17. **GPUs**: none before SB9 (no GPU nodes on the sandbox cluster).
18. **Kernel**: gVisor implements Linux in user space. Almost everything runs; some syscalls, `/proc` entries and kernel
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
| 400 | invalid input, or a refused field (`autoPause`, `volumeMounts`, `mcp`, `network.rules`, …) |
| 401 | missing, bad or revoked key |
| 403 | the key's creator can no longer manage sandboxes in the project; billing profile suspended; sandboxes not enabled for the project |
| 404 | unknown sandbox or template, a sandbox of another project, an endpoint that is not served |
| 409 | the project is DISABLED or being deleted |
| 429 | the project's sandbox quota |
| 503 | the provider is DISABLED, a release gate does not hold, the location does not run envd, the cluster is unreachable, or the sandbox did not start in time |

On sandbox URLs (`<port>-<id>.<domain>`), the proxy answers 502 for an unknown sandbox and 403 when the traffic access
token is missing or wrong.

## Compatibility suite

`tests/e2b-compat/compat.py` runs the stock SDKs end to end against a location (commands, streaming, exit codes, files,
preview URL, metadata, connect, egress rules, the metadata address, a second project's key, the code interpreter):
`E2B_API_URL`, `E2B_API_KEY`, optionally `E2B_OTHER_API_KEY` (a key of another project) and `SSL_CERT_FILE` for a
private CA.
