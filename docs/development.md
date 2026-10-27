# Development

Getting a ProxCloud backend and its SPAs running on your machine.

## Prerequisites

- **Docker** (with Compose) — the easiest way to run the whole stack, and
  required for the integration suite (testcontainers).
- **Go 1.25+** — the backend (`go.mod` pins `go 1.25`; module
  `github.com/mutex-solutions/proxcloud`).
- **Node 20+ / npm** — the two SPAs (`web/client`, `web/admin`) are Vite + React
  19, for running them with hot reload.
- An **OIDC issuer** (Keycloak or similar) and an **OpenStack region** only if
  you want to exercise authenticated or cloud-backed endpoints. A lot of local
  work needs neither — public endpoints and the management port work without them.

## Get the code and build

```sh
git clone <this-repo> && cd proxcloud
go build ./...      # compile everything
go vet ./...
go test ./...       # fast unit tests, no Docker (see testing.md)
```

## Run the whole stack with Docker Compose

The repo ships a `docker-compose.yml` at the root that builds and runs the three
ProxCloud images plus PostgreSQL and RabbitMQ:

```sh
docker compose up --build
```

That gives you:

| Service           | URL                     |
| ----------------- | ----------------------- |
| API (application) | http://localhost:8080   |
| API (management)  | http://localhost:8081   |
| Customer console  | http://localhost:8082   |
| Operator admin    | http://localhost:8083   |

PostgreSQL (`:5432`) and RabbitMQ (`:5672`, management UI `:15672`) come up as
dependencies. The API starts with `PROXCLOUD_JOBS_DEBUG_TRIGGERS=true`, so you can
drive jobs on demand (see below) without the crons running.

Compose does **not** bundle an OIDC issuer or an OpenStack region — auth-gated and
cloud routes need external ones. Put their connection details in a `.env` file
next to `docker-compose.yml`; the compose file reads them into the containers:

```sh
# .env — external OIDC issuer (for the SPAs) and OpenStack region (for the API)
PROXCLOUD_OAUTH2_ISSUER=https://id.example.com/realms/clients
PROXCLOUD_OAUTH2_CLIENT_ID=proxcloud-ui
PROXCLOUD_ADMIN_OAUTH2_ISSUER=https://id.example.com/realms/master
PROXCLOUD_ADMIN_OAUTH2_CLIENT_ID=proxcloud-admin
OS_AUTH_URL=https://keystone.example.com:5000/v3
OS_REGION_NAME=RegionOne
OS_USERNAME=...
OS_PASSWORD=...
OS_USER_DOMAIN_NAME=Default
OS_PROJECT_NAME=...
OS_PROJECT_DOMAIN_NAME=Default
```

To have the **API itself** validate customer/operator tokens (not just wire the
SPAs), add the `AUTH_MAIN_OAUTH2_ISSUER_URI` / `AUTH_ADMIN_OAUTH2_ISSUER_URI`
(and client-id) env to the `api` service — see
[configuration.md](configuration.md) for the full realm table. Without any auth
env the stack still runs; authenticated and cloud-backed routes stay unavailable.

Check the API is up:

```sh
curl :8081/actuator/health              # liveness
curl :8081/actuator/health/readiness    # UP once Postgres + Rabbit are reachable
```

### Run just the backend from source

For a faster backend edit loop, run the API with `go run` against the
compose-provided (or standalone) PostgreSQL and Rabbit:

```sh
PROXCLOUD_CONFIG_FILE=/nonexistent \
PROXCLOUD_DB_URL=postgres://proxcloud:proxcloud@localhost:5432/proxcloud?sslmode=disable \
PROXCLOUD_RABBITMQ_HOST=localhost \
PROXCLOUD_RABBITMQ_USERNAME=guest PROXCLOUD_RABBITMQ_PASSWORD=guest \
PROXCLOUD_ENCRYPTION_DEFAULT_KEY=dev-only-key \
PROXCLOUD_JOBS_DEBUG_TRIGGERS=true \
go run ./cmd/api
```

Useful env toggles (all default off; full table in
[configuration.md](configuration.md)):

- `PROXCLOUD_JOBS_SCHEDULER_ENABLED=true` — start the cron jobs (off by default so a
  dev instance never charges on a timer).
- `PROXCLOUD_JOBS_DEBUG_TRIGGERS=true` — expose on-demand job triggers on the
  management port (`POST :8081/debug/run-{sync,metrics,charge,…}`) without
  starting the crons — the deterministic way to drive jobs in dev.

## Seed base configuration

Fresh databases need the platform/billing config documents before the billing and
project endpoints work. The canonical documents live in `deploy/seed/`
(`platform-configuration.json`, `billing-configuration.json`). The
`deploy/seed/apply-seed.sh` script upserts those two singletons into a cluster's
datastore via `kubectl` (`external-service-dev.json` is a reference EXAMPLE for
creating dev providers through the admin Add-provider form/API — apply-seed.sh
does not seed it); for a local PostgreSQL, upsert the same JSON into the matching
document table (`id text primary key, doc jsonb`), e.g.:

```sh
psql "$PROXCLOUD_DB_URL" -c \
 "CREATE TABLE IF NOT EXISTS \"platformConfiguration\" (id text PRIMARY KEY, doc jsonb NOT NULL);
  INSERT INTO \"platformConfiguration\" (id, doc) VALUES ('seed', '$(tr -d '\n' < deploy/seed/platform-configuration.json)')
  ON CONFLICT (id) DO UPDATE SET doc = EXCLUDED.doc"
```

Repeat for `billingConfiguration` ← `billing-configuration.json`.

## Point at an OpenStack dev cloud

Two ways to give ProxCloud a cloud:

1. **Dev bootstrap via env** — set the standard `OS_*` variables (`OS_AUTH_URL`,
   `OS_REGION_NAME`, `OS_USERNAME`, `OS_PASSWORD`, `OS_USER_DOMAIN_NAME`,
   `OS_PROJECT_NAME`, `OS_PROJECT_DOMAIN_NAME`, or an application credential). The
   API authenticates in the background and exposes a read-only probe at
   `GET :8081/debug/cloud`.
2. **Register an external service** — insert a `CLOUD` `externalService` document
   (see `deploy/seed/external-service-dev.json` — an array with one example
   document per provider kind: `openstack` with `config.identityUrl`, regions,
   per-service enablement and the encrypted admin auth, and `kamaji` with the
   argocd/cluster blocks and the management kubeconfig secret; insert each
   element as its own row, replacing the `__…__` placeholders). This is how a
   real region is configured and supersedes the env bootstrap; sync and
   per-tenant clients use it. The kamaji provider also needs a prepared
   management cluster — see `docs/managed-k8s.md`.

## Run the SPAs with hot reload

Compose serves the built SPAs; for UI work run them from source with Vite HMR
instead:

```sh
cd web/client && npm install && npm run dev   # customer console, http://localhost:5173
cd web/admin  && npm install && npm run dev   # operator admin console
```

`npm run dev` starts Vite with HMR; `npm run build` type-checks and builds;
`npm run lint` runs the linter. Configure each SPA's API base URL and its OIDC
realm (issuer, client id, PKCE) to point at your local API
(`http://localhost:8080`) and IdP. The API's CORS allow-list is derived from its
`PROXCLOUD_SELF_UI_BASE_URL` / `PROXCLOUD_SELF_ADMIN_BASE_URL` plus
`PROXCLOUD_CORS_ALLOWED_ORIGINS`, so add your dev origin (`http://localhost:5173`)
there when calling cross-origin.

## The edit/run loop

- Backend logic: `go test ./...` (+ `go vet`) on every change; add integration
  coverage when you touch storage or a multi-step flow
  ([testing.md](testing.md)).
- Driving jobs: run with `PROXCLOUD_JOBS_DEBUG_TRIGGERS=true` and `POST` the
  `/debug/run-*` triggers instead of waiting on cron.
- SPA: `npm run dev` — Vite HMR reloads on save.
- Full stack: `docker compose up --build` after a change rebuilds the images.
- Chart: `make deploy` runs `helm upgrade --install` (see
  [ADR-0005](adr/0005-self-contained-helm-chart.md)).

## Where things live

- `cmd/api` — the server entrypoint and all wiring.
- `internal/platform/*` — the business domains (org, project, billing, pricing,
  payment, admin, audit, job, promotion, mcp, …), each as handler/service/repo.
- `internal/cloud` — the OpenStack facade: client, resource cache, providers,
  sync/metrics/notification pipelines.
- `internal/{config,server,health,pgdoc,amqp,oidc}` — wiring and infrastructure.
- `pkg/{auth,httpx,money,audit,textcrypt}` — cross-cutting: auth middleware, the
  response envelope + request context, money⇄decimal string, the audit helper,
  field-level encryption.
- `deploy/` — `Dockerfile`, the Helm chart (`chart/`), values overlays,
  and `seed/`.
- `web/client`, `web/admin` — the two React SPAs.
- `test/integration` — the testcontainers suite (`-tags=integration`).
