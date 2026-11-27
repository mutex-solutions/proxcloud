# Managed functions

A function is a customer's container image behind a stable internal HTTPS URL, run as gVisor pods on the
sandbox provider's cluster. This page says what the portal does for a function, what it stores, and what it
does not do yet.

**State of this page: the portal's API is built and switched off.** No sandbox provider can have functions
enabled in this release (the provider save refuses `config.sandbox.functions.enabled: true`), so the routes
below answer `403` to everything but a read and a delete, and there is nothing to read. The parts that are
still missing are listed under [What is not built yet](#what-is-not-built-yet). Nothing on this page has run
on a cluster: every statement about the cluster was tested against an in-memory one
(`internal/platform/project/cloud_function_integration_test.go`), every statement about records against a real
PostgreSQL.

## What a function is

- One `apps/v1` Deployment `fn-<16 hex>` in the namespace `fn-<project>` of the sandbox provider's cluster.
  The cluster side (the pod, the namespace, its quota, the two Secrets) is
  [`internal/cloud/function`](../internal/cloud/function).
- Reached at `https://<name>-<6 hex>.<the provider's sandbox domain>`. The host is given once and never again:
  every host ever issued stays in `function_hosts`, also after its function is deleted, so a URL never reaches
  another project's function.
- From an image the customer names, in a registry the sandbox machines reach. The registry's host must be a DNS
  name (the pull is made by the machine itself, outside every pod network policy). Every start asks the
  registry, for a tag and for a digest.
- **Not offered**, and refused by name with `400` rather than taken and ignored: a GPU (`gpu`), a build from a
  repository (`source`), a public endpoint (`exposure: public`, `publicRateLimit`), a hostname of its own
  (`domains`). Their routes do not exist.

## Who can call a function, and what it gets

This is the request path as it is built in `sandbox-proxy` (its function mode, off until the chart sets
`FUNCTION_DOMAIN`); nothing of it has run.

- **An endpoint is internal, and has no caller authentication.** The host resolves to the sandbox proxy's
  address, which is an address of the platform network. Whoever can open TCP 443 there and knows the host name
  can call the function. The six hex digits of the host are not a secret. A function that needs to know its
  caller checks a header itself.
- **Every tenant's functions are under one parent domain.** A function must not set a cookie for that parent
  domain: another project's function would be sent it.
- **Each instance gets 1 GiB of its own disk** (`ephemeral-storage`, request and limit) beside its CPU and
  memory; one that writes more is evicted.
- **What an instance may reach on the network** is set by the cluster's policies for function namespaces. A
  function namespace carries the tenant label of a sandbox namespace, so the sandbox chart's pod policy and egress
  guardrail select it; the policy that opens the internet for function pods is not in the chart yet.

## The API

All under `/api/v1/project/{id}`, with the project's own permissions: `project:cloud_resource:read` for the
reads, `project:cloud_resource:manage` for everything else. No new permission.

| Route | What it does |
|---|---|
| `GET /functions` | The project's functions, with the live number of ready instances |
| `POST /functions` | Create and deploy |
| `GET /functions/{fn}` | One, with what an unfinished rollout or start is waiting for (`reason`) |
| `PATCH /functions/{fn}` | Change it. `image`, `port`, `cpuMillis`, `memoryMi`, `env`, `command`, `args`, `secrets`, `registryAuth`: a new deployment. `minInstances`, `maxInstances`, `idleSeconds`, `concurrency`: no deployment |
| `DELETE /functions/{fn}` | Stop it and remove it (`202`) |
| `GET /functions/{fn}/deployments` | Its deployments, newest first. Ten are kept, and the one that serves |
| `POST /functions/{fn}/rollback` | `{deploymentId, registryAuth?}`: roll back to a deployment that once served |
| `POST /functions/{fn}/action` | `{action: "suspend" \| "resume"}` |
| `GET /functions/{fn}/logs?tail=` | The newest log lines of its instances (200 by default, 5000 at most) |

A function's `status` is derived: `DEPLOYING` (a rollout is in progress), `READY` (an instance is ready),
`IDLE` (none is), `SUSPENDED`, `FAILED` (nothing healthy was ever deployed). A later deployment that fails
leaves the function on the one that served, with `lastError` set.

**Defaults of a create:** port 8080, 250m CPU, 256 MiB, `maxInstances` 1, `minInstances` 0, the provider's idle
time (300 s), `concurrency` 50. `maxInstances` is 1 unless the customer raises it, on purpose: whoever reaches a
function's address keeps it awake, and with more instances allowed drives it to that many, and the project pays.

**A rollback runs the bits that ran.** A deployment records the digest its instances ran when it became active
(`imageDigest`). A rollback to a deployment that named a tag deploys `<repository>@<that digest>`, whatever the
tag points at since. Its secrets and registry credentials are the ones the function has now: they are not
versioned. A LATER instance of the CURRENT deployment under a tag may still pull a tag that moved: name a digest
where that matters.

**A failed start is explained in one of nine fixed sentences** (`reason`, `lastError`, a deployment's
`message`): for example "image pull failed: check the image reference and that the registry allows anonymous
pulls". What the cluster itself said (the kubelet's message of a failed pull, the scheduler's of a pod without a
machine) names machines and internal addresses; it goes to the portal's log for the operator (`function: rollout
failed`, with `detail`) and into no answer, record or audit event.

### Where functions are off

| State | Answer |
|---|---|
| The portal has no function service | `503` "functions are not configured", on every route |
| The provider's `functions` block is off or not valid, the provider is `DISABLED`, or the project has no sandbox provider | `GET` routes and `DELETE` are answered, everything else is `403` "functions are not offered here" |
| A release gate of the provider does not hold | create, a deployment, a rollback and a resume answer `503`; the reason stays the operator's |
| The project cannot be billed (disabled, in deletion, its billing profile suspended) | the same four are refused (`403` or `409`) |

An update that raises `minInstances` or `maxInstances` passes the same two checks as a deployment; one that
lowers them, or changes only `idleSeconds` or `concurrency`, does not.

Reads and the delete stay open so that what is left where functions were switched off can be seen and removed.
The service a project's console reads (`GET /project/{id}/service`) carries a `functions` block, with the limits
the create form checks against, only where functions are offered (neither of the first two rows applies): the
menu entry and the routes hide together.

## Secrets and registry passwords

- A function's `secrets` are environment variables the API takes and never gives back. `PATCH` merges them (a
  string sets a name, `null` removes it, a name not given stays); `env` is replaced as a whole.
- `registryAuth {server, username, password}` is one thing: an object sets or replaces the credentials (with the
  password, every time), `null` removes them, absent keeps them. Credentials for another registry than the
  image's are refused.
- **At rest** each value is sealed with the portal's encryption key (`pkg/textcrypt`, the key behind the
  portal's `encryption.defaultKey` setting), bound to its project, its function and its name: a blob copied to
  another function's record, to another name, or into the place of the registry password does not open, and a
  function whose stored values do not open is not deployed (`503`), with nothing sent to the cluster.
- **Without a key** every write that carries a secret or a registry password answers `503` and stores nothing.
  A function without secrets needs no key.
- **Nowhere else.** No route returns a value. An answer shows the names (`secretNames`), when each was set
  (`secretsSetAt`) and, of the credentials, the server, the user and when the password was set. Deployment
  records, the cloud row, audit events (which carry the function's id, name, image, deployment number and the
  NAMES of its secrets) and log lines hold no value. A request body that does not decode is answered "invalid
  request body" and never quoted. `env` values are shown to those who may change the function; others see the
  names.
- **On the cluster** the values are in two plain Secrets per function (`<id>` and `<id>-registry`), owned by the
  Deployment. A Secret is not encrypted at rest by anything this product configures, and in a platform with its
  own sandbox cluster that cluster's datastore is on the platform cluster. A kubelet reads the Secrets of the pods
  on its machine: code that escapes gVisor on a sandbox machine reads the secrets and registry passwords of every
  function with an instance there, other projects' too.
- **Whoever may read the project's resources may read a function's logs.** A function that prints its secrets
  shows them to every member.

## Quota

A project's function quota is three numbers: `functions`, `vcpus`, `ramMb`. Each is the project's own
(`functionQuota`, set by the operator), else the provider's `config.sandbox.functions.projectQuota`, else the
built-in 20 functions, 8 vCPUs and 16384 MiB. Never unlimited: an absent key falls back. The number of
functions is also held to the provider's `maxFunctions`.

- **What a function holds is reserved, not measured:** `cpuMillis` and `memoryMi` times its `maxInstances`,
  whether it runs or sleeps, and the larger of its two sizes while a deployment rolls out. So a request the
  portal lets wake a function is never refused by the namespace's own quota.
- Checked at a create, at an update or rollback that raises what the function holds, and at a resume (`429`).
  A change that does not raise it is never refused, so a project over a quota that was lowered can still make
  its functions smaller or delete them. Of several creates at the limit only as many as fit succeed.
- The namespace's ResourceQuota `proxcloud-function` is written from the same three numbers before anything
  may start an instance (a create, a deployment, a change of the scale settings, a resume). It is the backstop:
  at the limit a rollout's one extra instance does not fit, the new deployment fails with "the instance could
  not be created (project quota or cluster admission)" and the old one keeps serving. It is never written under
  what the project's functions already hold: a quota an operator lowers under that refuses what is new in the
  portal, the instances of what was admitted before still start, and the namespace follows down as the
  functions get smaller.
- `GET /api/v1/project/{id}/quota-usage` on the sandbox service answers `functions: {limits, usage}`.
- The operator sets a project's numbers with `PUT /api/v1/admin/project/{id}/function-quota`
  (`{functions, vcpus, ramMb}`, each optional; an empty body removes them). They are **not** part of the
  project's quota document, and `PUT /api/v1/admin/project/{id}/quota` refuses `functions`, `functionVcpus`,
  `functionRamMb` and `functionQuota`: a key in that document that is not a sandbox key makes it a Proxmox
  quota, in which every absent key is unlimited.

## The record, the cloud row, and what billing will read

- `functions`: one record per function (configuration, host, the sealed values, the deployment that serves and
  the one in progress, the instance count, the running intervals). `function_deployments`: one per rollout.
  `function_hosts`: every host ever issued.
- Each function has one cloud resource row of type `FUNCTION`, a projection of its record: `name`, `host`,
  `url`, `image`, `cpuMillis`, `memoryMi`, `instances`, `billedInstances`, `status`
  (`RUNNING`, `IDLE`, `SUSPENDED`, `DELETED`), `runningSince`, `stoppedAt`, `endedIntervals
  [{from, to, instances, cpuMillis, memoryMi}]` (kept ten minutes).
- **The record and its row change in one transaction**, at every boundary of a running interval and at every
  change of the size that is billed. One function (`scaleFunction`) writes the instance count, under the
  record's lock: of two writers of the same start or stop exactly one makes a boundary.
- **A delete archives the row at once**, marked `DELETED` with `stoppedAt` set when the function was running.
  No row lingers among the live ones. A create that could not be started leaves an archived row with no running
  interval at all. A project's deletion does the same for every function, after deleting
  the project's function namespace on each sandbox provider; where a namespace cannot be deleted nothing is
  dropped, the project stays in deletion and the deletion job tries again.
- **Nothing bills a function yet.** No billing resource reads `FUNCTION` rows in this release.

## What is not built yet

Each of these is a later part of the same work; until all are in, functions cannot be switched on.

- **Scale to zero and wake.** Nothing stops an idle function and nothing starts a sleeping one on a request:
  the reaper, the wake and activity routes of the proxy, scaling on load. `minInstances`, `idleSeconds` and
  `concurrency` are stored and do nothing yet.
- **The jobs that keep record and cluster equal.** A rollout is settled by the API process that started it. A
  process that ends meanwhile leaves the function claimed by that rollout: it stays `DEPLOYING`, and a new
  deployment is refused ("a deployment of this function is already in progress") until the job that settles
  such rollouts exists. A Deployment with no record is not swept either.
- **The enabling gate**: the checks under which an operator may switch the provider's block on.
- **Billing**: the meter of `FUNCTION` rows and its price rule.
- **The cluster's side of a platform**: the role `proxcloud-function-namespace`, the policies and the proxy's
  function mode in the sandbox chart, and the platform run that sets them up.
- **Consoles and tools**: the customer console's page, the operator's forms, the MCP tools.

## Upgrading and rolling back

Nothing changes for an installation that upgrades: three empty collections and their indexes are created at
start. A portal rolled back to a release without this code ignores them; no `FUNCTION` row can exist, because
no function can be created while the provider's block cannot be switched on.
