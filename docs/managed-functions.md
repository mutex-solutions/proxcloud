# Managed functions

A function is a customer's container image behind a stable internal HTTPS URL, run as gVisor pods on the
sandbox provider's cluster. This page says what the portal does for a function, what it stores, and what it
does not do yet.

**State of this page: the portal's side is built and cannot be switched on yet.** A sandbox provider's
functions block is accepted only through [its gate](#switching-functions-on), and one row of that gate asks for
a `sandbox-proxy` that reports to this portal with the provider's function wake token. No platform this release
can build has such a proxy (the sandbox chart with the function objects is not pinned), so the row cannot hold,
the routes below answer `403` to everything but a read and a delete, and there is nothing to read. The parts that
are still missing are listed under [What is not built yet](#what-is-not-built-yet). Nothing on this page has run
on a cluster: every statement about the cluster was tested against an in-memory one
(`internal/platform/project/cloud_function_*_integration_test.go`), every statement about records against a
real PostgreSQL. Scaling on load has never run on a cluster anywhere.

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
| The job scheduler is off (`PROXCLOUD_JOBS_SCHEDULER_ENABLED`) | the same four answer `503`: nothing would stop an idle function or charge a running one |
| The project cannot be billed (disabled, in deletion, its billing profile suspended) | the same four are refused (`403` or `409`) |

An update that raises `minInstances` or `maxInstances` passes the same two checks as a deployment; one that
lowers them, or changes only `idleSeconds` or `concurrency`, does not.

Where functions are off (the second row) every function of the provider is stopped by the next run of the
reaper, within 15 seconds, whatever its traffic and its `minInstances`, and nothing of it is billed from then
on. The functions, their deployments and their addresses are kept; they run again when functions are offered
again (one with a floor at once, the others on their next request).

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

## Scale to zero and wake

A function runs between its floor and its `maxInstances`. The portal alone writes the count: the tenant's API,
the proxy's wake, the load and the two jobs all go through one function (`scale` in
`internal/platform/project/cloud_function_scale.go`), so the billing check, suspension and the row that is billed
have one writer.

- **Idle.** `lastActiveAt` on the function's record is the only source. Each `sandbox-proxy` replica reports
  every 5 seconds which functions had a request or hold one. A function idle for its `idleSeconds` (default 300,
  held to the provider's `minIdleSeconds` and `maxIdleSeconds` at the moment of the check) goes down to its
  floor: `minInstances`, at most the provider's `maxMinInstances`, at least the provider's `minInstances`, and
  zero for a function that never had a deployment become ready.
- **Wake.** A request for a function without a ready instance is held by the proxy, which asks the portal to
  start one and waits up to 50 seconds for it. The portal sets the count to one and answers at once.
- **Load.** With `maxInstances` above 1 a function runs `ceil(requests in flight / concurrency)` instances, the
  requests in flight being the sum over the replicas heard from within two of their report periods. Up at once,
  down one instance per 60 seconds and only while no report asked for every instance. One API replica decides
  per provider at a time (a PostgreSQL advisory lock, tried and not waited for). `concurrency` is a target for
  this sum and no limit per instance: the proxy picks an instance at random.
- **A stop that meets activity yields.** The reaper decides on a fresh read; when a wake or a report stamped
  the function between that read and the write, nothing is stopped and the cluster is put back to the recorded
  count.

### The two routes `sandbox-proxy` calls

Both are in the main router beside the sandbox wake route, reachable without a user credential, and answer in
the portal's envelope. The bearer is the provider's **function wake token** (`secret.functionWakeToken`), a
token of its own: `secret.proxyWakeToken`, the bearer of the sandbox wake route, is never compared here, and the
function token is refused there. The token is checked before anything is looked up. Bodies are read up to
64 KiB. The providers' tokens are kept in memory for 3 seconds, so a replaced token is still taken for that
long on each API replica.

`POST /api/v1/internal/functions/{functionID}/wake`

| Answer | When |
|---|---|
| `200` `{functionID, instances}` | the function is at one instance or more (a second wake changes nothing) |
| `401` | no bearer in the `Bearer` form, or one that is no provider's function wake token; the same answer whatever was asked for |
| `404` | no such function for this token: an unknown id, or the function of another provider |
| `403` | functions are off where it lives, it is suspended, or its project cannot be billed |
| `409` | nothing to run: no deployment ever became ready and none is in progress |
| `429` | the cluster refuses the instance for the namespace's quota (`Retry-After`) |
| `503` | the job scheduler is off, a release gate does not hold, or a read or the cluster failed (`Retry-After`) |

`POST /api/v1/internal/functions/activity` with `{ids, every, replica, partial, load: {id: {peak, requests}}}`
answers `204`. It stamps `lastActiveAt` of the named functions **of the token's provider only**, keeps the
replica's report (unless `partial`) and then scales that provider's functions to the load. At most 1000 ids and
1000 loads; `replica` is a pod name.

What the token can do: keep every function of its provider awake, and through the load at its `maxInstances`,
which its owner pays for; and write the report the gate below reads. It reads nothing and changes no function's
configuration. Rotating it is an apply of the installation.

## The jobs that keep record, row and cluster equal

Both run on the job scheduler ([jobs-scheduling.md](jobs-scheduling.md)), under a lock, once across the API
replicas. Run twice on the same state, the second run changes nothing.

**`functionReaper`**, every 15 seconds. Beside the idle rule it stops every function of a provider that no longer
offers functions and of a project that cannot be billed, and raises a function below its floor.

- An idle function is ended **at the moment its idle time ran out** (`lastActiveAt` plus the idle time, never
  before the interval's start), not at the moment the reaper got to it. A reaper that was away, because the
  scheduler was off or the portal down, does not make the owner pay for the time nobody looked.
- The first run after a gap of 45 seconds or more stops nothing for being idle: the reports that keep serving
  functions awake were away as long. They arrive every 5 seconds, so the next run decides on fresh stamps.
  A run that finds no function keeps no record of itself (a site without functions is read, never written), so
  the first run that finds one counts as a run after a gap too.
- A stop is recorded whatever the cluster answers; see below.

**`functionDeploySync`**, every minute at second 20, before the charge at second 30:

- settles a rollout whose API process ended (the function held its claim for 3 minutes per instance);
- writes a cloud row again that no longer says what its record says, and ends and archives one whose record is
  gone;
- scales a Deployment to the recorded count, either way. This is also what repeats a stop the cluster did not
  take;
- deletes a Deployment without a record, and applies again, from the record and at the recorded count, a
  Deployment that is missing (with its Secrets and, when it is gone too, the namespace and its quota): a sandbox
  cluster that was rebuilt gets its functions back. Not where functions are off;
- deletes a function namespace whose project is gone, and one that holds no function record and is older than
  10 minutes;
- settles whether a running interval serves (next section), and takes instances the cluster cannot place beside
  a ready one off the count;
- forgets `sandbox-proxy` replicas not heard from for 90 seconds;
- stamps the provider's meter heartbeat after each good list of its Deployments, and not otherwise.

### A start that never serves

A wake, a resume or a first deployment starts an instance and a running interval. Whether it serves is not
known then: the machines may have no room, the namespace's quota or admission may refuse the pod, the image may
not pull, the container may keep exiting. A held request counts as activity, so the idle rule alone would keep
such a function "running" for as long as its callers retry.

- The record carries `readyAt` per running interval: stamped when a rollout finishes with instances running,
  and by the deploy sync when it sees a ready instance. A start removes it.
- The deploy sync notes a running interval without a ready instance on one run (`unreadyAt`) and **ends it on
  the next**, a minute later: within two minutes of the start.
- An interval that never had a ready instance is ended as **void**: it joins no ended interval, and the record
  and the row keep no bound of it. Nothing of it is ever billed. So is the start of a first deployment that
  fails.
- An interval that had a ready instance and lost it is ended like any other, and billed to that moment.
- The next request starts the function again; while nothing becomes ready that start is ended the same way. A
  function with a floor of one is started again by the reaper: one attempt every two minutes, none billed.

### A cluster that is gone or does not answer

- **A stop is recorded even when the cluster call fails**, and the cluster's error is returned after that. The
  record and the row say zero from that moment; the deploy sync repeats the call until the Deployment is at
  zero. For the time between, instances may run that nothing bills.
- **The meter heartbeat** of the provider (`meterState.lastOkAt`) moves only when the deploy sync listed the
  provider's Deployments. Nothing reads it for functions yet: the meter that writes off the time of a provider
  not seen for 30 minutes is not built.
- **A platform run that deletes the cluster** ends the intervals first: `EndFunctions` ends every running
  interval of a provider in the records without any cluster call, and `FunctionsExist` tells whether a provider
  holds a function record. Both are in the portal for the platform run to call; nothing calls them yet.

### Machine rotation

Read in the code, not run. With a sandbox cluster of its own (`separate`), the rotation of sandbox machines waits
for every pod on the machines it replaces: a running function instance holds a rotation back, under a message
that names sandboxes. With the shared node group the rotation looks at sandbox namespaces only and does not wait
for function instances: they end with their machine and the Deployment starts them on another.

## Switching functions on

`config.sandbox.functions.enabled: true` is accepted by the provider save only while these hold. A save that
fails one answers `409` and names it; `POST /api/v1/admin/service/{id}/sandbox/release-check` lists the rows
after the provider's release gates wherever the provider holds a function wake token or has the block on.

| Row | Holds when |
|---|---|
| `function-scheduler` | the job scheduler is on: the reaper, the deploy sync and the minute charge are scheduler jobs |
| `function-sealing-key` | the portal has an encryption key: secrets and registry passwords are stored sealed |
| `function-proxy` | a `sandbox-proxy` replica of this provider sent this portal an activity report in the last 90 seconds |
| `function-billing` | billing is not the external provider (`PROXCLOUD_BILLING_PROVIDER=external`): it is not sent a function's running intervals and would bill its idle time |
| `function-price` (a warning, refuses nothing) | an enabled price plan has a rule for resource type `function` |

- **`function-proxy` is the evidence that the wake path works at this site.** A report is taken only with the
  provider's own token, so one on record means the proxy runs the function mode, holds the token, reaches the
  portal, and the portal's routes answer it. An empty report counts: a proxy reports before any function
  exists. The records are kept exactly as long as the row counts them.
- **It is asked at the save that switches the block on**, also for a provider that is `DISABLED`. A later save
  of a provider whose functions are on is not refused because its proxies were restarting at that moment: the
  only answer to that refusal would be to switch functions off, which stops them all. The other three rows are
  asked at every save that leaves the block on.
- **A new provider is never created with functions on**: no proxy can have reported for an id that did not
  exist.
- **Without a price rule functions run and bill nothing.** A rule added later bills a running function back to
  the start of the billing cycle or to its start, whichever is later.
- **Off is always accepted**, and stops every function of the provider within 15 seconds.

## MCP tools

Nine tools of the tenant toolset reach the nine routes above: `list_functions`, `get_function`,
`create_function`, `update_function`, `delete_function`, `list_function_deployments`, `rollback_function`,
`function_action` (suspend, resume) and `get_function_logs`. They go through the same routes with the caller's
own credential, so the same permissions, gates and quota apply. No tool takes a GPU, a source to build from, a
public endpoint or a hostname. A secret value and a registry password given to a tool land in the agent's
conversation. The tool dispatch writes no log line of a call; the request it makes is logged like any API
request (method and path), never its body.

## The record, the cloud row, and what billing will read

- `functions`: one record per function (configuration, host, the sealed values, the deployment that serves and
  the one in progress, the instance count, the running intervals). `function_deployments`: one per rollout.
  `function_hosts`: every host ever issued. `function_proxies`: the last complete report of each `sandbox-proxy`
  replica per provider (kept 90 seconds), and the reaper's own last run (written only while a function exists).
- Each function has one cloud resource row of type `FUNCTION`, a projection of its record: `name`, `host`,
  `url`, `image`, `cpuMillis`, `memoryMi`, `instances`, `billedInstances`, `status`
  (`RUNNING`, `IDLE`, `SUSPENDED`, `DELETED`), `runningSince`, `readyAt`, `stoppedAt`, `endedIntervals
  [{from, to, instances, cpuMillis, memoryMi}]` (kept ten minutes).
- **`readyAt` is what a meter must wait for.** A running interval without it is one nobody has seen serve: it
  is charged from `runningSince` once `readyAt` is stamped, or once it ends some other way than as void. A void
  interval leaves no `runningSince` and no `stoppedAt` on the row, and no entry in `endedIntervals`.
- **The record and its row change in one transaction**, at every boundary of a running interval, at every
  change of the size that is billed, and when `readyAt` is stamped. One function (`scale`) writes the instance
  count, under the record's lock: of two writers of the same start or stop exactly one makes a boundary.
- **A delete archives the row at once**, marked `DELETED` with `stoppedAt` set when the function was running.
  No row lingers among the live ones. A create that could not be started leaves an archived row with no running
  interval at all. A project's deletion does the same for every function, after deleting
  the project's function namespace on each sandbox provider; where a namespace cannot be deleted nothing is
  dropped, the project stays in deletion and the deletion job tries again.
- **Nothing bills a function yet.** No billing resource reads `FUNCTION` rows in this release.

## What is not built yet

Each of these is a later part of the same work; until all are in, functions cannot be switched on.

- **Billing**: the meter of `FUNCTION` rows, its rule for a provider whose cluster was not seen (the heartbeat
  above), and its price rule.
- **The cluster's side of a platform**: the role `proxcloud-function-namespace`, the policies and the proxy's
  function mode in the sandbox chart, and the platform run that sets them up, mints the function wake token and
  calls `EndFunctions` before it deletes a cluster.
- **The operator's forms** for the provider's functions block and the gate's rows.

Known limits of what is built, to be measured on a lab before functions are offered:

- Scaling on load has not run on a cluster, and `concurrency` is not enforced per instance.
- Record and cluster can differ for up to a minute after two writers raced in opposite directions, until the
  deploy sync runs.
- Instances the cluster cannot place **beside a ready one** are billed for one to two minutes, until the deploy
  sync takes them off the count.
- A scale-down ends requests that run longer than the pod's grace period.
- A proxy replica that is killed loses up to one report period of activity.
- After a gap, a replica whose first report takes longer than one reaper period to arrive can lose a serving
  function to the idle rule: it is woken again by the next request.

## Upgrading and rolling back

Nothing changes for an installation that upgrades: four empty collections (`functions`,
`function_deployments`, `function_hosts`, `function_proxies`) and their indexes are created at start, and two
scheduler jobs run that find nothing to do. No `FUNCTION` row can exist while the provider's block cannot be
switched on.

**Before a portal is rolled back to a release without functions, once functions exist:** switch the functions
block of every sandbox provider off and wait until no function runs (one run of the reaper, 15 seconds; `GET`
of a function shows `instances: 0`). The older portal has no reaper and no deploy sync: an instance left running
would run without end, unbilled and unstopped, and a project that holds a `FUNCTION` row could not finish its
deletion there. Roll forward again, switch the block on, and the functions run as before.
