# Holder operations: bounded observation and durable uncertainty

## Why a timeout cannot stand alone

Proxmox tasks continue after a request is cancelled or its task-status read fails.
An API worker can also lose its PostgreSQL connection while its Go callback keeps
running. PVE does not accept a fencing token. Therefore neither a session lock,
a renewed TTL nor a successful-looking empty VM config makes compensation safe.
Destroying an apparently empty carrier can delete a disk that lands a moment later.

The holder paths couple two mechanisms:

1. **Admission lease**: PostgreSQL `operationLease`, a fresh UUID owner, retained
   monotonic epoch, 90-second DB-clock lease renewed every20seconds. Renew, check,
   release and operation admission compare this exact acquisition. They lock a row
   in a short transaction and evaluate the DB clock *after* acquiring that lock.
   Each database call is bounded to5seconds; acquisition waits at most60seconds.
2. **Nonexpiring dispatch intent**: `pveHolderOperation`, unique unsettled operation
   per provider/project pool. `DISPATCHING` commits **before** the mutation request;
   the actual UPID is appended only after receiving and saving the response.
   Lease expiry never expires this row. A successor may take the lease but can
   only observe existing work, not POST it again or execute a new request.

No external call runs inside a database transaction. No long-lived connection or
ShedLock extension is used. A failed renewal cancels local work and prohibits every
subsequent guarded mutation, including cleanup with `WithoutCancel`. Pending work
prevents release; if the process disappears, its lease expires but its intent stays.
This deliberately trades availability for safety.

## Observation budgets and outcomes

The additive `WaitTaskBounded` API distinguishes stopped-OK, stopped-failed and
uncertain. Only adapted holder config/create/power/move operations use the5minute
observation budget; shared-storage carrier migration uses15minutes. Node identity
and effect settlement is bounded to60seconds. These are observation budgets, not
PVE task kill deadlines. The service's `HolderBudgets` allows explicit operator
configuration/embedding overrides; the admin poll endpoint cannot extend them.

Generic `WaitTask` and heavy create/clone/import/backup/retype copy budgets remain
unchanged. Retype has a durable intent and keeps the existing caller-context wait.
Cancellation, timeout,403,404, lost POST response, malformed/null UPID or failed
UPID persistence leaves the project held. **No task is killed; no carrier, new
holder or legacy blank VM is destructively compensated.**

Known UPID recovery polls only. Even a retry that successfully settles previous
work returns409 requesting a refresh; it never falls through to allocate/move a
second disk. Settling requires a stopped task and fresh node-scoped SMBIOS UUID,
positive pool membership, and operation-specific disk/config/power readback.
Holder-only proof readers require an actual pool object with the exact `poolid`
and an explicit nonnull `members` array with valid member identities. Missing/null
payloads, wrong pools, malformed members and missing/null protection/pending
inventories are unknown, never evidence of absence. Generic client convenience
readers keep their existing normalization semantics; they are not proof readers.
New holders send an unguessable SMBIOS UUID in their first CREATE. VMID reuse and
blankness are not identity. The normal pool-scoped Config.Options authorization
must admit that parameter; a refusal stays safe, not an automatic permission grant.

Delete settlement also accepts a precisely bound stopped-OK `qmdestroy` task
(node/type/exact VMID), previously read UUID and a positively authorized fresh
pool read omitting that VMID. Destroy removes pool-derived VM.Audit, so a subsequent
node-config403 alone is **not** deletion proof. Exact canonical node/VMID missing
config plus positive VM.Audit remains supported; a broad substring/cluster census
is never proof. External administrator races remain an operator trust boundary:
PVE has no external fencing token, and these guards do not serialize root actions.

Synchronous HA/rule/replication DELETEs used by retirement also commit intent before
dispatch; an acknowledged synchronous success plus fresh identity/effect readback
can settle in that original worker. A lost response has no UPID and stays held.
There is no guess based on TTL or absent task history.

## Covered entrypoints and boundaries

- Detached create; holder/carrier create/config; volume add, extend, detach,
  attach/reassign, keep, delete and retype; empty-holder cleanup.
- `KeepServerVolumes` admits **before** reading disks, even an initially empty VM.
- `RetireServer` admits before protection removal, stop or backup deletion; nested
  helpers retain the same session. Its protection mutations use the intent adapters.
- `DeleteServer` admits before destructive work; direct holder deletion through a
  server action is refused. Teardown explicitly permits holders under the same fence.
- `DeleteProjectBackups` admits before the project's backup-delete precursor;
  `TeardownProjectPool` independently admits before any pool-member stop/delete.
- The sync's blank cleanup admits but never auto-destroys legacy blank VMs. Their
  age/config is insufficient to exclude an unknown CREATE. Inspect them manually.
- Production project, admin and sync factories all receive the durable store.
  Standalone tools and old unit-test fixtures may omit it and retain legacy behavior;
  do not construct an unfenced factory in an HTTP deployment.

This is not a generic PVE operation engine. Seed/NoCloud journals, import/backup
workers, scheduler locks, inventory/DNS behavior and ordinary heavy task waits are
not migrated. Out-of-band low-level client calls and external administrators are
not fenced by this holder protocol. Concurrent portal operations on a server outside
these holder/delete entrypoints are not a generalized per-server operation lock.

## Administrator status and poll-only recovery

Read status first (requires `admin:service:read`):

```text
GET /api/v1/admin/service/{provider}/holder-operations/{project}
```

The response is payload-free journal evidence (`pending`, `operatorHold`), including
operation ID, UUID owner/epoch, expected nodes/VM identities, disk/config fingerprints,
state and UPID when known. It contains no seed content, credentials or task logs.

To observe **that exact operation**, requires `admin:service:manage`:

```text
POST /api/v1/admin/service/{provider}/holder-operations/{project}/poll?operationId={id}
```

This never dispatches a PVE mutation. A stale operation ID requires another status
read. Known task still running/unreadable or insufficient readback remains held.
No UPID means an operator hold, not an automatically retryable request. There is
**no force-clear or automatic unknown-clear API**.

For an unknown/no-UPID hold, retire the original API worker (all replicas that could
still dispatch that acquisition), inspect PVE task history on the exact nodes and
verify UUIDs/disk ownership/effects. Preserve the journal and database backup. An
operator with direct database authority may resolve only after documenting positive
proof and independently reviewing the exact operation; absence/elapsed TTL/blank VM
is not proof. When proof cannot be obtained, keep the project held and escalate.
Do not delete the journal row or invent a UPID to make a retry succeed. A future
proof-bearing administrative resolution workflow is deliberately not part of R1.

## Regression evidence

Scoped Go1.25 tests include fake PVE running/failed403404, lost/empty/malformed
responses, failed UPID save, VMID/UUID mismatch, recovery without resubmission,
normal disk effects and cleanup no-destroy assertions; deterministic logical time
passes10minutes while renewal continues and verifies cancellation/goroutine exit.
Real PostgreSQL tests use two concurrent owners, retained epoch/UUID CAS,
crash-after-intent across TTL, exactly-once **settlement** (not external dispatch),
observed row-lock waits past expiry and termination of the actual blocked reply-save
backend. Database partition is modeled by a cancelled save and backend termination;
no claim is made that a live cloud/PG network partition was injected.

Lab destructive/node-failure drills are not part of these tests. The main operator
runs one small non-GPU VM lifecycle only after existing seed fixtures are gone and
after independent review/full gates; never kill a shared lab node to test this code.
