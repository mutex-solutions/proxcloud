# Move jobs from RabbitMQ to PostgreSQL without losing historical work

Fresh installs need **only a writable PostgreSQL primary** for application data and
durable jobs. There is no broker connection, broker readiness check, or queue-enable
switch. This release wires the engine and native billing jobs; **mail is not a durable
outbox yet**. External billing's HTTP calls and failure behavior are unchanged.

The planned broker-free release is **v0.4.0, unpublished until separately approved**.
Use a core-containing source build (`images.build: true`) or a known compatible
new API image; do not deploy this chart against old v0.3.x binaries. Render/lint
success does not prove that the planned package/image is published.

This is an **operator-controlled cutover**, not a queue importer or broker cleanup.
Neither a false `rabbitFanout` flag nor an empty-looking console proves a safe drain.
The new API does not consume old RabbitMQ messages. Do not upgrade an active producer
fleet and assume its old queue will drain on the new workers.

## 1. Record the old state, then quiesce and reconcile

Use your real context/namespace/release, broker node and vhost throughout. Commands
below are examples to run **only after reviewing their targets**; the installer does
not execute the mutation steps for you.

1. Back up PostgreSQL and its encryption key. Save the old application image digest,
   chart/version, values, mounted `application.yml`, broker definitions and exact
   broker resource/container identity. Do not publish credentials in a ticket or
   paste DSNs into values examples. Record all broker volumes/PVCs, their PV reclaim
   policies and a restorable snapshot/backup. A definitions export alone does **not**
   back up queued messages; use an operator-approved RabbitMQ data backup procedure.
2. Stop **all old producers**: every old scheduler replica, external cron/automation,
   manual job trigger and debug producer. Block manual trigger access during the
   maintenance window. Disable timed work first, but leave the old consumer version
   running temporarily if it can safely finish the queue. Do not clear the old fanout
   flag while it is still your only consumer path. A config change normally needs a
   restart; plan the separate producer/consumer shutdown using the old version's
   actual wiring, not an assumed independent worker process.
3. Read **both** `messages_ready` and `messages_unacknowledged` in **every relevant
   vhost/queue**, plus `consumers`. For example, inside the existing broker pod or
   container, use `rabbitmqctl list_queues -p <vhost> name messages_ready
   messages_unacknowledged consumers`. Save timestamped output (including
   `proxcloud.charge` and any retry/dead-letter queues). Do not purge a queue to make
   the numbers zero. After producer shutdown, allow known-safe old consumers to
   drain; repeat the observations over a full in-flight/old-producer interval.
4. **Reconcile ambiguous billing work instead of replaying it at today's time.**
   Legacy messages contain `profileId` and `timeUnit`, but no original cutoff/cycle.
   The original cycle cannot be invented or recovered from those two fields alone.
   Check billing watermarks, bills and external-side effects against the original
   scheduled run, broker backlog and operator records. Resolve uncertain units under
   a finance-approved procedure and record each decision. Never convert an old
   message into a current-cutoff job merely to empty the queue. Native financial
   commits and any external billing HTTP effects must be reconciled separately.
5. Stop **every old consumer/API process** after the drain/reconciliation. Repeat
   queue observations: `ready=0`, `unacked=0`, `consumers=0` for a completed drain,
   with no old producers able to resume. If a consumer was killed while holding work,
   wait for it to be requeued, observe again, and reconcile it. A reconciliation with
   messages intentionally retained needs an explicit per-message ledger and snapshot;
   it is not a zero-queue claim. Save the final evidence and sign-off. Do not run old
   and new billing schedulers/consumers concurrently.
6. Only now clear `PROXCLOUD_JOBS_RABBIT_FANOUT` and YAML
   `proxcloud.jobs.rabbit-fanout`. A true value is a deliberate startup migration
   error. Old broker host/credential settings are ignored with a warning; remove them
   from the API rather than mounting unnecessary broker secrets.

Keep the evidence in a non-secret operator ticket/path. The deployment gates require
that reference and affirmative `producersStopped`, `consumersStopped`, and
`drainedOrReconciled` acknowledgments. These are **human attestations**, not a broker
probe or a cryptographic proof. Revalidate them before the first deployment; retain
the record for later upgrades. A fresh site has no old queue and leaves migration
mode empty on install (record the equivalent no-legacy state for later Helm upgrades).

## 2. Protect a Kubernetes broker BEFORE changing desired resources

Deleting a `RabbitmqCluster` can cascade to its children, including data volumes.
**Do not `kubectl delete` the old cluster, its PVCs, namespace or operator.** A Helm
annotation added only to a new chart manifest is too late if the old resource is
omitted. ArgoCD pruning is a different deletion path from Helm.

First pause automated sync/pruning of the old ArgoCD Application (or other controller
that would revert metadata), save its prior settings, and manually annotate the
**existing live** CR before upgrading:

```sh
# Verify context, namespace and exact existing name first.
kubectl -n <namespace> get rabbitmqcluster <existing-name> -o yaml
kubectl -n <namespace> annotate rabbitmqcluster <existing-name> \
  helm.sh/resource-policy=keep \
  argocd.argoproj.io/sync-options=Prune=false,Delete=false --overwrite
```

Merge with any existing Argo sync options rather than discarding them. Verify the
annotations persisted, and take/verify the volume backup/snapshot. The keep policy
protects release removal, **not** explicit `kubectl delete`, operator reconciliation,
namespace deletion, or storage administration. Keep the operator while the retained
cluster exists. Do not rely on the storage class's reclaim default as your backup.

### Helm: two release phases, never a one-step omission

Helm compares against the **previous release's stored manifest**, so a live-only
annotation is not sufficient. Use the same release/name/namespace and old secrets:

1. Before the new binary cutover, protect a running release using the **old chart
   reference and old API image**, with a reviewed annotation-only post-renderer
   (or equivalent transition chart) that keeps the exact old broker enabled and
   adds the keep annotation to its stored release manifest. Verify it with
   `helm get manifest`; do not rely on a new chart that changes the API to v0.4.0
   before old work is reconciled/stopped. Keep old automation paused as above.
2. **Retain transition with the new API:** after section 1, upgrade to this chart with
   `jobsMigration.mode=retain`, the existing `rabbitmqCluster.enabled=true` and
   **the exact old cluster name/spec** (replicas, image, storage and resources).
   Keep any custom spec using `rabbitmqCluster.retainedSpec` (a full spec map).
   Supply the evidence/acknowledgments below. This records a CR with
   `helm.sh/resource-policy: keep` in Helm's stored release manifest and protects it
   against Argo pruning too. The new API uses PostgreSQL, not this retained broker.
   Do not change the broker image, storage or placement as part of this step.
3. Inspect `helm get manifest <release> -n <namespace>` and the live CR; both must
   contain keep. Verify the broker/PVC identities and queue evidence again.
4. **Broker-free desired state (optional later):** only after that successful
   transition use `jobsMigration.mode=cutover`, `retentionPrepared=true` and both
   `rabbitmqCluster.enabled=false` / `rabbitmq.enabled=false`. The broker is omitted
   from desired resources but retained in the cluster; **it is not deleted**.
   Preserve its snapshot and ownership records outside Helm for recovery.

Example transition values (**no credentials**):

```yaml
jobsMigration:
  mode: retain
  evidence: ops-ticket-123/queue-and-volume-backup
  producersStopped: true
  consumersStopped: true
  drainedOrReconciled: true
rabbitmqCluster:
  enabled: true
  name: existing-release-rabbitmq  # MUST be the old live name; keep its full old spec
api:
  jobs:
    schedulerEnabled: false
    debugTriggers: false
```

Do not blindly use `--reuse-values`: it can retain `rabbitFanout=true`, a dev
subchart toggle or old broker dependencies. Review `helm get values --all`, supply
your saved secrets, and render the exact migration values before upgrading. Even
an installation with no legacy broker must explicitly attest that fact with
cutover mode/evidence on its first Helm upgrade; Helm cannot prove its history from
values alone. Keep those migration values for subsequent upgrades.

**Old Bitnami broker subcharts:** keep `rabbitmq.enabled=true` in retain mode with
its **same image, credentials, persistence and values** until a separate manual
retention plan covers every managed object. This chart cannot add keep to every
historical subchart child. Manually protect the StatefulSet, PVCs and needed
Services/Secrets/ConfigMaps in **both live objects and a stored transition release
manifest** (a reviewed Helm post-renderer is one option); verify before omission.
Stage 40 refuses to automatically remove an existing enabled Bitnami broker.
It also refuses old Argo `helm.values`, `valueFiles`, `parameters`, `fileParameters`
and multi-source Application layouts until a separate manual retention review:
these can hide effective broker settings that a false `valuesObject` field does
not override. Independent StatefulSet inventory rejects a Rabbit-named/labeled
broker without a positively identified owning live RabbitmqCluster, even if the
Rabbit CRD is absent. Do not rename/hide workloads or just strip old values to
bypass this safety check.
Never switch a historical subchart directly into a new `RabbitmqCluster` or delete
its old PVCs as part of this migration.

### Installer / ArgoCD

Stage 40 no longer installs, upgrades, waits for or deletes the RabbitMQ operator.
It still deploys cert-manager for ingress/TLS and the CSI gateway. Before **any**
apply it reads existing Application/CR state and fails closed on API/RBAC errors.
Existing sites must fill `proxcloud.jobs_migration` in their site inventory:

```yaml
proxcloud:
  jobs_migration:
    mode: retain                # or cutover after protection is prepared
    evidence: ops-ticket-123/queue-and-volume-backup
    producers_stopped: true
    consumers_stopped: true
    drained_or_reconciled: true
    retention_prepared: false   # true for cutover; does NOT authorize deletion
    legacy_cluster_name: proxcloud-rabbitmq
  jobs:
    scheduler_enabled: false
    debug_triggers: false
```

For retain, the named CR **must already exist**. Stage 40 preserves its live spec;
for cutover it omits the CR, but requires the live Helm keep **and** Argo
`Prune=false,Delete=false` annotations first. Argo uses chart rendering, not Helm
release upgrade semantics: annotations on a rendered object alone do not protect
an object already pruned by the old Application. Stop/review the old Application's
reconciliation before the manual annotation step and restore sync only with the
new protected desired state. Existing operators, clusters and PVCs are left in
place. Teardown is a separate, explicitly approved runbook after the retention
period, not an installer cleanup operation.

## 3. Docker Compose: preserve the old container and anonymous data volume

The historical service is still named `rabbitmq`, with the **same**
`rabbitmq:3-management` image and no changed volume mapping. It is available only
with `--profile legacy-rabbitmq`. The old definition relied on the image's
**anonymous** `/var/lib/rabbitmq` volume. A new named mount would hide existing data;
this migration intentionally does not add one.

Before changing containers, record `docker compose ps -a`, `docker inspect` of the
old broker (mount/volume IDs, hostname/node name and image digest), and make a
verified backup. Complete section 1 against the **existing** broker and processes.
Do not remove/recreate it, renew anonymous volumes, change its node identity or
assume that starting a fresh container will recover the old queue.

After drain/reconciliation and old-consumer shutdown, replace **only** the API:

```sh
# Build/load the new API first; review the same project name and .env as the old stack.
docker compose up -d --no-deps --build api
```

Do not use `docker compose down`, `down -v`, `rm`, `--remove-orphans` or
`--renew-anon-volumes` during cutover. Do not remove PostgreSQL data. For inspection
of an already running legacy broker, use `docker compose --profile legacy-rabbitmq
exec rabbitmq ...`; `--profile` is not permission to recreate it. If it was stopped,
start the **existing container** only after verifying its identity/mounts. No Compose
command in this runbook deletes or moves historical data.

## 4. Verify the new workers before enabling new timed work

- `PROXCLOUD_DB_URL` must reach the **writable primary**. Chart-derived CNPG uses
  `<cluster>-rw`, never `-ro`. Jobs, leases, retries, deduplication and finance all
  use it; do not place the queue database on a read replica.
- Optional `PROXCLOUD_DB_READ_URL` is **history/metrics reads only**. Empty or missing
  falls back to primary; a configured but unavailable reader returns 503 for those
  reads, not a silent fallback. Finance, worker input and financial writes never use
  the replica. Replication lag can make status stale.
- `cnpg.statusReadReplica` defaults false; `-ro` is generated **only** with that
  opt-in and at least two instances. With `api.existingSecret`, the operator supplies
  its optional `db-read-url`; old Secrets missing it still work. External readers use
  `externalPostgresql.readExistingSecret` / `readSecretKey`, never an inline DSN
  password example. Installer opt-in is `database.status_read_replica`.
- Workers start on every API pod **even with scheduler/debug off** and resume
  already-enqueued jobs. Stopping timed producers is not a queue pause. Stop all
  worker/API processes during a maintenance pause; do not invent `QUEUE_ENABLED`.
- Defaults per pod: concurrency **2** (`1..16`), poll **1s** (`100ms..1m`), lease
  **1m** (`10s..10m`). Env names are `PROXCLOUD_JOBS_WORKER_CONCURRENCY`,
  `PROXCLOUD_JOBS_POLL_INTERVAL`, `PROXCLOUD_JOBS_LEASE_DURATION`; Helm uses
  `api.jobs.workerConcurrency`, `pollInterval`, `leaseDuration`. The bounded custom
  engine uses owner+epoch fencing and database-time compare-and-swap; a crash may
  cause another attempt, not permission for duplicate financial effects.
- Confirm readiness without RabbitMQ, job progress and finance outcomes. Review any
  failed/retrying job before redrive. Mail remains on its existing send path in this
  release: do not claim email survives a crash until the separate outbox is wired.
- Explicitly enable `schedulerEnabled` / installer `jobs.scheduler_enabled` only
  after the operator accepts the cutover. Debug triggers stay off unless deliberately
  used on an isolated management port. External billing remains HTTP-based and is
  not silently switched to native rating on errors.

Rollback is not a blind image downgrade: first stop new producers/workers and
reconcile PostgreSQL jobs, bills, external billing effects and the retained broker.
A rollback that re-enables old consumers or replaying both queues can bill twice.
Keep all snapshots and migration evidence until finance and operations approve
separate historical-data retirement.
