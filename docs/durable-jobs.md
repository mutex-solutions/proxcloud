# PostgreSQL durable native billing jobs and transactional enqueue

## Scope and choice

This runtime replaces RabbitMQ for **native billing dispatch and an isolated
selftest only**. Mail/invite delivery is still best-effort and is NOT a durable
outbox until a separate integration wires its business commits and SMTP handler.
SSE is pod-local and this queue is not a broadcast/event-stream substitute.
Deployment broker removal is a separate migration; this PR does not remove live
queues, charts, installer resources, containers or PVCs.

A small internal `durablejobs` module uses existing pgx v5 and pgdoc, with just two
fixed versioned job kinds. River v0.26.0 and v0.44.0 were inspected (the latter is
Go 1.25 compatible, MPL-2.0 core). Their `JobSetStateIfRunningMany` SQL and executor
completion/retry paths check id/running state, not an expected worker acquisition
and unexpired lease; a superseded worker can finalize a newer attempt. Fencing
only the business callback would not fence River's later completion/rescue. Rather
than fork/trigger another engine, this narrow DB-only financial path uses explicit
owner UUID + epoch + DB-clock CAS. No new Go toolchain, broker or dependency added.

## Safety contract

- All claims, renewals, retries, ACKs, replay authorization, receipts, pricing,
  profile locks, and current business data read/write use `PROXCLOUD_DB_URL` on
  **PRIMARY**. `pgdoc.Connect` validates recovery/read-only on every new connection
  and fails startup before DDL/workers on a standby or read-only session.
- Each API starts two workers by default (`PROXCLOUD_JOBS_WORKER_CONCURRENCY`,
  1..16). The scheduler remains OFF by default; workers process only accepted jobs.
  Poll defaults to 1s with empty/error jitter; lease defaults to 1m (10s..10m).
  SQL-only handler transactions have a 3/4-lease context deadline; final DB-clock
  expiry CAS rolls back even if a handler finishes after its lease. Claim recovery
  and bounded attempts cover a process crash. Renewal cannot resurrect an expired
  owner; row-lock waits are followed by a fresh database time check.
- A billing payload is `{version:1,profileId,timeUnit,cycleAt}`. Key = profile +
  cadence + ORIGINAL canonical UTC cutoff; it never uses processing time. Queue
  dedup does not replace payloads. Worker decoding rejects unknown fields/kinds,
  altered identity and malformed cadences. No arbitrary code/commands are jobs.
- The whole profile's **all service** mutations, receipt, and ordinary queue ACK
  are one primary transaction joining pgdoc `WithTx` under the existing profile
  money lock. Callback effects are database/pure math only. Transient config/read
  errors are propagated, not turned into an empty successful financial receipt.
- The independent `billingChargeReceipt` survives queue retention and protects a
  committed business write followed by a crash before ACK. Watermarks never rewind;
  deletion intervals and monthly engine clock honor the original cutoff.
- Current cache/price attributes are resolved at execution time. This is NOT a
  historical pricing/resource snapshot/replay system. Missing intervals require
  reconciliation. No external HTTP, SMTP, card or cloud exactly-once claim.
- SENT/PAID cycles never reopen and closing work cannot silently acknowledge a
  missing nonempty bill. Such work returns reconciliation errors and eventually
  becomes visible dead letters with no receipt/ACK. Invoice issuance under the same
  primary profile lock defers on pending/running/dead jobs for that original cycle.
  A producer that arrives only after issuance is rejected by the issued-cycle guard;
  manual reconciliation, not implicit invoice mutation, is required.
- External billing keeps its previous HTTP behavior and does not silently fall
  back to native or get new automatic external retries. External workers claim only
  selftests; native backlog stays pending/unclaimed. Drain/reconcile before changing
  providers; never run mixed native/external producer configurations.

## Transactional outbox primitive

Prepare schema before serving. Inside a business transaction:

```go
err := db.WithTx(ctx, func(tc context.Context) error {
    if _, err := db.C("business").InsertOne(tc, business); err != nil { return err }
    _, err := jobs.EnqueueTx(tc, durablejobs.Selftest, key, payload, 3)
    return err
})
```

`EnqueueTx` REQUIRES the same DB's transaction context, never calls `Pool.Begin`
behind the business write. `db.Tx(tc)` is a controlled raw-pgx bridge and must not
be committed/rolled back by callers. Wrong-DB contexts are rejected at **every**
pgdoc SQL boundary (including reads and advisory locks), not silently redirected
or downgraded to another pool. Rollback gives no job and no business mutation;
commit makes both durable. The first phase proves this primitive, not mail wiring.

## Status, selftest and replay

- `GET /api/v1/admin/durable-jobs`: `admin:bill:read`; primary authorization first,
  then bounded lag-tolerant counts and 50 recent rows; payload/owner excluded.
- `POST /api/v1/admin/durable-jobs/{id}/replay`: `admin:bill:manage`, body only
  `{"reason":"confirmed repaired cause"}` (8..500 characters). UUID ID, known job,
  dead state and immutable payload validated on PRIMARY; reason/principal/previous
  attempts audit commits in `durable_job_replay` with dead→pending. No payload edit,
  arbitrary command, replay-success job, or hidden force operation. The standard
  admin audit middleware also records the authenticated HTTP mutation.
- With debug triggers or scheduler on: `POST :8081/debug/jobs-selftest` verifies
  enqueue→running worker→ACK without billing fixtures, and `jobs-status` reports
  history. Keep management port private as before. `run-charge` now means durable
  enqueue acceptance, not synchronous completion. Old rabbit-selftest/fanout debug
  endpoints are gone.

`PROXCLOUD_DB_READ_URL` opens a separate max-two-connection read-only history pool.
Empty uses primary **for history only**. Configured-unavailable history returns
503 without affecting financial workers. Responses say `lagTolerant:true`, source,
observed DB time and optional WAL replay timestamp/seconds. This lag is not a bound
on data age (an idle primary may produce no WAL). No auth/security reads or replay
validation uses this handle. This setting does NOT deploy physical replicas.

## RabbitMQ migration — never drop an unknown backlog

1. Inventory the installed version, all replicas, producer schedule/debug callers,
   `PROXCLOUD_JOBS_RABBIT_FANOUT`, broker messages-ready/unacknowledged and consumers.
   Preserve broker volumes and export evidence. Do not uninstall the broker first.
2. Stop old timed/manual producers. Drain old consumers **before** upgrading them;
   retain exact ready/unacknowledged/consumer counts and billing watermarks/issued
   cycle evidence. Old payloads contain only profile/unit, no original timestamp;
   do NOT invent a timestamp or bulk-republish them as today's cycle.
3. Reconcile remaining legacy messages with authoritative schedule/log/bill evidence.
   If original cutoffs are unknown, hold the migration for operator review rather
   than promise an import. No automatic destructive drop or overlap backfill exists.
4. Stop old workers; confirm no mixed versions/engines remain. Upgrade all replicas
   with scheduler OFF, clear the retired fanout flag, keep correct primary DSN.
   `PROXCLOUD_JOBS_RABBIT_FANOUT=true` now fails startup with a drain/reconcile message;
   legacy Rabbit host/credentials alone are ignored with a secret-free warning.
5. Run isolated selftest/status, confirm primary-only claims and migration evidence;
   separately authorize enabling cron producers. Explicitly reconcile any old/new
   overlap or issued-month late work. Never run old Rabbit consumers alongside new
   PostgreSQL workers for a billing migration.
6. Remove default broker deployment dependencies in the deployment PR only after the
   drain/reconciliation attestation. Do not delete an existing queue/PVC just because
   the new application no longer connects to it. Rollback also requires stopping
   producers and reconciling accepted PostgreSQL jobs, not reverting to Rabbit blindly.

## Attempts, retention and vacuum

Attempts are 8 for billing, 3 for selftest, max20 at the store boundary; retries
use exponential backoff plus jitter, bounded below15m. Invalid payloads become dead
immediately; transient handler failure/crash exhausts to dead. Errors stored in the
queue are fixed safe classifications, not raw provider strings/credentials.

`Store.Prune(keep)` removes at most500 **succeeded** rows per call, minimum24h;
recommended keep7d and operator-controlled periodic invocation. It is not an
implicit admin deletion API. Dead letters, independent financial receipts and
replay audits are never automatically deleted. Deleting financial receipts while
allowing historical publication/replay loses dedupe protection; archive and reconcile
under a separate approved retention policy first. Unresolved dead billing jobs block
invoice issuance and must not be hidden by deleting evidence.

Partial indexes cover pending schedule, running expiry, financial profile backlog
and succeeded/dead completion; unique `(kind,key)` covers publication. Monitor queue
age/counts, dead rows, `pg_stat_user_tables.n_dead_tup`, autovacuum/analyze times,
transaction duration and connection saturation. Keep autovacuum enabled; tune per
workload with the DBA. Bounded deletes generate WAL and do not shrink files immediately;
use normal VACUUM/ANALYZE scheduling. Do not use blocking VACUUM FULL on a live queue
without an outage plan. Job/receipt backup consistency is database consistency; queue
acceptance is not an off-site backup or synchronous-replication durability promise.

## Reproducible tests (only disposable local PostgreSQL)

```sh
export PATH=/home/mishuhashi/.local/share/mise/installs/go/1.25.14/bin:$PATH
export GOTOOLCHAIN=local
export GOCACHE="$R/.claude/go-build"
go test ./internal/durablejobs ./internal/pgdoc ./internal/platform/billingjob ./internal/platform/chargefanout ./internal/platform/pricing ./internal/config ./cmd/api
go test -tags=integration ./test/integration -run 'TestDurable|TestPgdocOwned|TestPrimaryStartup' -count=1 -v
```

Real PostgreSQL tests exercise competing claimers, duplicate publishing, leases
and stale owner denial, expiry-mid-transaction rollback, crash recovery, poison
and audited replay, worker selftest/cancellation, transactional enqueue visibility,
whole-profile two-provider rollback, business-commit-before-ACK replay without
extra money/units, completed payload retention with retained receipt, provider-mode
backlog preservation, two actual DB transaction isolation, read-only startup,
monthly clocks/deletion delay across month boundaries and issued-cycle/invoice gates.
Physical streaming-replica topology is separate operator evidence, not simulated
by a read-only-session unit test or automatically deployed by this PR.
