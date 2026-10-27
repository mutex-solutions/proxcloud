# 7. Run scheduled jobs under a PostgreSQL distributed lock, with optional broker fan-out

## Status

Accepted

## Context

The API runs its background work — usage rating/charging, cloud metrics
ingestion, resource sync, dunning/suspension, savings-contract expiry,
transaction reconciliation, project deletion — *inside the same process* as the
HTTP server, on a cron timer (`internal/platform/scheduler`, wired in
`cmd/api/main.go`). That keeps operations simple: one deployable, one thing to
scale.

But we also want to scale the API **horizontally** for request throughput and
availability. The moment there is more than one pod, every pod's cron fires the
same job at the same instant. For rating that is not a cosmetic bug — it would
double-charge customers. We need horizontal scaling for the request path and
**exactly-once** execution for the job path, without standing up a separate
scheduler service.

## Decision

Guard every scheduled job with a **PostgreSQL-based distributed lock** over a
`shedLock` table (`internal/platform/lock`). Each job has a lock name, a
`lockAtMostFor` ceiling (auto-release if the holder dies), and a `lockAtLeastFor`
floor (minimum spacing so a fast job can't immediately re-run). When the cron
fires on every pod, all of them attempt the lock; exactly one acquires it and
runs, the rest no-op. The lock is a single `INSERT … ON CONFLICT (id) DO UPDATE`
keyed on the job name — a live lock makes the conflict arm's `WHERE lockUntil ≤
now` guard fail, so competing acquirers affect zero rows and lose the race
atomically in the database.

The scheduler (`robfig/cron` with seconds-precision specs) and the lock are
composed in `Scheduler.RunLocked`, which acquires, runs iff acquired, and
releases honoring the floor.

For the one job that is both hot and naturally shardable — the per-profile charge
run — provide an **optional RabbitMQ fan-out** (`internal/platform/chargefanout`,
gated by `PROXCLOUD_JOBS_RABBIT_FANOUT`). Instead of one pod charging every profile
in a loop, the locked cron *publishes one message per active billing profile*;
any pod's consumer drains the queue, charging one profile per message. The
per-profile charge math is identical to the in-process path. Default is off
(in-process loop).

The whole scheduler is **off by default** (`PROXCLOUD_JOBS_SCHEDULER_ENABLED`), so a
plain deploy is dormant and never charges bills on a timer until explicitly
enabled.

## Consequences

- The API scales out for requests while jobs still run once across the fleet;
  no dedicated scheduler/worker deployment is needed.
- The lock lives in the database we already run — no new infrastructure
  (ZooKeeper/etcd/Redis) for coordination.
- `lockAtMostFor` is a safety ceiling: if a pod dies mid-job the lock frees after
  it, so the job can run again — jobs must therefore be **idempotent**, which they
  are designed to be.
- Fan-out isolates a slow or failing profile from the rest and spreads charge
  work across pods, at the cost of requiring a healthy broker; if fan-out is on
  but the broker is down, the tick falls back to charging in-process so work is
  never silently dropped.
- Two execution modes (in-process loop vs. broker fan-out) exist for the charge
  job; both share the same per-profile logic, so correctness is not duplicated.
- Enabling jobs is a conscious operational step, which prevents a surprise
  billing run on a freshly deployed or test environment.
