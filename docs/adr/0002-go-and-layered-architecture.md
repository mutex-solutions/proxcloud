# 2. Go with a handler → service → repository layering over PostgreSQL

## Status

Accepted

## Context

The platform has to do three kinds of work at once and keep doing it reliably:

- serve two browser SPAs and a machine-facing API (customer console, operator
  admin, public admin API);
- run a large amount of concurrent I/O against an OpenStack cloud — polling
  resource state, reconciling a local cache, driving lifecycle actions across
  many tenants;
- run scheduled background jobs (rating/charging, metrics ingestion, dunning,
  cleanup) on a timer inside the same process.

We wanted a stack that produces a single, self-contained deployable, has
first-class concurrency for the cloud-sync and job workloads, and keeps
operations simple (no runtime, no application server, fast startup, small
image). We also wanted a codebase a new contributor can navigate by following an
HTTP route straight down to the database.

## Decision

Implement the backend in **Go** (currently Go 1.25, see `go.mod`) as one binary,
`cmd/api`, that serves both the application router (`:8080`) and a management
router (`:8081`).

Route HTTP with **chi** (`github.com/go-chi/chi/v5`). The application router
(`internal/server`) mounts one handler group per domain under `/api/v1`, plus
`/admin-api/v1`; the management router serves health and operator debug
triggers.

Organize each domain as three layers:

- **Handler** — HTTP concern only: decode the request, resolve the caller from
  the request context, enforce policy, call the service, encode the response
  envelope. (e.g. `org.Handler`, `project.Handler`.)
- **Service** — business logic and orchestration, storage-agnostic (e.g.
  `org.Service`, `billing.ActivationService`).
- **Repository** — all PostgreSQL access for one aggregate; owns table names,
  indexes, and JSON/JSONB mapping (e.g. `org.Repo`, `project.Repo`). Nothing above
  the repo layer touches the driver (`internal/pgdoc`).

Wiring is explicit constructor injection in `cmd/api/main.go` — no DI container.
Dependencies a package cannot import directly (to avoid import cycles) are passed
as small function values or interfaces (e.g. the org handler receives a project
member-adder via a setter). Concurrency uses the standard library: goroutines
for background connection maintenance and OIDC/cloud discovery, `context` for
cancellation, `sync/atomic` for the hot-swappable cloud and broker clients.

## Consequences

- One static binary, no external runtime. Startup does not block on
  RabbitMQ, the IdP, or the cloud — each is dialed in the background and gated by
  readiness — so a dependency being briefly unreachable does not deadlock a
  rollout. (PostgreSQL is the exception: it is connected and pinged eagerly, so a
  missing database fails the boot.)
- The layering is a convention enforced by review, not by the compiler. The
  payoff is that any endpoint can be read top-to-bottom in one domain package.
- Explicit wiring makes `main.go` long, but it is the single, greppable source
  of truth for how the system is assembled.
- Go's error-as-value style and lack of exceptions means every failure path is
  visible at the call site; handlers translate errors into the response envelope
  and HTTP status.
- Committing to Go means the team standardizes on Go tooling (`go test`,
  `go vet`, modules) for the backend build chain.
