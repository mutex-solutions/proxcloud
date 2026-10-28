# Testing

ProxCloud has two test layers: fast **unit tests** that need nothing external, and
a **hermetic integration suite** that runs against a throwaway PostgreSQL started
via testcontainers. Both run with plain `go test`; the integration suite is
behind a build tag so the default run needs no Docker.

## Unit tests

Pure-logic tests live next to the code they cover (`*_test.go`), especially in
the money and pricing paths — e.g. `pkg/money`, `internal/pgdoc`
(`codec_test.go`), and `internal/platform/pricing`
(`adjustments_test.go`, `bill_build_test.go`, `billagg_test.go`). They exercise
rating math, the DECIMAL128-precision rating math, adjustment tiers, and the SigV4 canonicalizer
(`pkg/auth/sigv4_test.go`) without any I/O.

Run them:

```sh
go test ./...      # or: make test
go vet ./...       # or: make vet
```

No Docker, no PostgreSQL, no network. This is the loop to run on every change.

## Integration suite

The integration tests live in `test/integration/` and are gated by the
`integration` build tag (the first line of each file is `//go:build
integration`), so they are excluded from `go test ./...` and only run when you
ask for them:

```sh
go test -tags=integration ./test/integration/...   # or: make test-integration
```

### What it needs

A working Docker engine. `TestMain` (`test/integration/main_test.go`) starts a
`postgres:17-alpine` container with testcontainers-go, connects through `internal/pgdoc`,
and tears the container down at the end — you do **not** provide or seed a
database yourself. Each test gets an isolated, uniquely-named database via the
`freshPG(t)` helper (`it_<TestName>`), which drops it on cleanup, so tests don't
bleed into each other.

On **Windows with Docker Desktop**, point testcontainers at the Docker Desktop
Linux engine and disable the reaper sidecar:

```sh
DOCKER_HOST=npipe:////./pipe/dockerDesktopLinuxEngine \
TESTCONTAINERS_RYUK_DISABLED=true \
go test -tags=integration ./test/integration/...
```

### What it covers

The suite drives real repository and service behavior against real PostgreSQL — index
enforcement, JSON/JSONB round-tripping, and the multi-step flows that unit tests can't
reach. Representative areas (one file each, `test/integration/`):

- **Platform / identity** — org members and roles, custom-role unique index,
  project deletion (`platform_test.go`, `project_deletion_test.go`).
- **Billing & payments** — bill DTO shaping, pay-a-bill, add-funds, card collect,
  bank transfer, the collect job, the transaction scanner, and payment emails
  (`billdto_test.go`, `paybill_test.go`, `payment_*_test.go`).
- **Pricing** — rating and charge runs, bill build/aggregation
  (`pricing_test.go`, `pricing_bill_test.go`, `pricing_charge_test.go`).
- **Cloud** — resource sync, reconcile, billing-resource metering, the metrics
  and sync pipelines (`cloud_*_test.go`).
- **Jobs & lifecycle** — the distributed lock, the scheduler,
  activation/validation, suspension, savings expiry, reminders, and mail jobs
  (`lock_test.go`, `scheduler_test.go`, `activation*_test.go`,
  `suspension_test.go`, `savings_test.go`, `reminder_test.go`,
  `mail_jobs_test.go`, `metricsjob_test.go`, `billingjob_test.go`).

## Adding a test

- **Unit test:** add `X_test.go` next to the code, no build tag, package `X`.
  Prefer this whenever the logic can be exercised without I/O (rating, DTO
  mapping, validation, encoding).
- **Integration test:** add a file to `test/integration/` starting with
  `//go:build integration`, package `integration`. Get a database with
  `db := freshPG(t)`, construct the repository/service under test with it (e.g.
  `org.NewRepo(db)`), call `EnsureIndexes` if the behavior depends on an index,
  and assert against real reads. The shared `postgres:17-alpine` container is already up;
  don't start your own.

Keep tests deterministic: pass an explicit `now time.Time` into anything
time-dependent (the scheduler and rating paths accept an injected clock for
exactly this) rather than reading the wall clock inside the assertion.
