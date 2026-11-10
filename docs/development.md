# Development

Getting a ProxCloud backend and its SPAs running on your machine.

## Prerequisites

- **Docker** (with Compose) — the easiest way to run the whole stack, and
  required for the integration suite (testcontainers).
- **Go 1.25+** — the backend (`go.mod` pins `go 1.25`; module
  `github.com/mutex-solutions/proxcloud`).
- **Node 24 / npm** — use the frontend CI toolchain for the two Vite + React 19
  SPAs (`web/client`, `web/admin`), with `npm ci` and each committed lockfile.
  Do not assume an arbitrary Node 20 installation satisfies the current Vite engines.
- An **OIDC issuer** for authenticated endpoints (Compose bundles Keycloak on
  `:8090`) and, for cloud-backed endpoints, a reachable **Proxmox VE** cluster
  with an API token. A lot of local work needs neither — public endpoints and the
  management port work without them.

## Get the code and build

```sh
git clone <this-repo> && cd proxcloud
go build ./...      # compile everything
go vet ./...
go test ./...       # fast unit tests, no Docker (see testing.md)
```

## Run the whole stack with Docker Compose

The repo ships a `docker-compose.yml` at the root that builds and runs the three
ProxCloud images plus PostgreSQL (including durable jobs) and a dev Keycloak:

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

PostgreSQL (`:5432`) is the data/job dependency; no broker starts by default.
Scheduler and debug triggers both default **false**. To drive jobs deliberately
on this isolated dev stack, set `PROXCLOUD_JOBS_DEBUG_TRIGGERS=true` in `.env` and
replace the API. Workers always finish already-enqueued work even with both flags
off. Existing broker users must follow the [migration runbook](postgres-jobs-migration.md)
and preserve the old container's anonymous volume, not run `down -v`.

Keycloak (`:8090`) imports the dev realm `proxcloud` from
`deploy/dev/keycloak-realm.json`, with two users, `demo`/`demo` and
`admin`/`admin`. The API and both SPAs are already wired to it. The browser reaches
Keycloak at `localhost:8090` (that URL becomes the token's `iss`); the API reaches
the same realm at `keycloak:8080` inside the compose network, hence the
`*_DISCOVERY_URI` variables in `docker-compose.yml`. No SMTP is configured, so a
self-registered dev user must be marked "Email verified" in the Keycloak admin
console.

To use an external issuer instead, override the `PROXCLOUD_*OAUTH2_*` variables in
a `.env` file next to `docker-compose.yml`; see
[configuration.md](configuration.md) for the full realm table. Cloud routes stay
unavailable until a Proxmox provider is registered (below).

Check the API is up:

```sh
curl :8081/actuator/health              # liveness
curl :8081/actuator/health/readiness    # PostgreSQL readiness; no broker dependency
```

### Run just the backend from source

For a faster backend edit loop, run the API with `go run` against the
compose-provided (or standalone) **writable-primary** PostgreSQL:

```sh
PROXCLOUD_CONFIG_FILE=/nonexistent \
PROXCLOUD_DB_URL="$PROXCLOUD_LOCAL_PRIMARY_DSN" \
PROXCLOUD_ENCRYPTION_DEFAULT_KEY=dev-only-key \
PROXCLOUD_JOBS_DEBUG_TRIGGERS=true \
go run ./cmd/api
```

Load `PROXCLOUD_LOCAL_PRIMARY_DSN` from your local secret store; do not commit
credentials. `PROXCLOUD_DB_READ_URL` is optional history/metrics only and empty
means primary fallback, never a worker/finance replica. Worker defaults are 2
(`1..16`), poll `1s` (`100ms..1m`), lease `1m` (`10s..10m`).

Useful env toggles (both default off; full table in
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

## Point at a Proxmox VE cluster

Cloud providers are not configured through env; they are `externalService`
records.

1. **Create a privilege-separated API token** on the cluster with the
   `ProxCloudPlatform` role. [proxmox-permissions.md](proxmox-permissions.md)
   has the `pveum` commands. Turn the datacenter firewall on and run every node
   on proxmox-firewall (nftables); test connection refuses a cluster without
   them.
2. **Register it** in the admin console: System → Cloud providers → Add provider →
   Proxmox, fill in the API URL, token ID and secret (plus a certificate
   fingerprint for a self-signed cluster), then **Test connection**. Alternatively
   insert a `CLOUD` `externalService` document with `config.provider =
   "proxmox"`; the document shape is in
   [cloud-integration.md](cloud-integration.md#the-proxmox-provider-document).
3. Enter a project in the customer console: it is bootstrapped onto the provider
   (pool, default security group, default quota). Drive a sync with
   `POST :8081/debug/run-sync`.

### Seed an operator setup

After registering a Proxmox provider (admin console, **Cloud providers**), one command
gives it what customers need: platform configuration and region, flavor catalog, billing
configuration, the public price plan with its compute, volume and public IP rules, and the
provider default quota. It goes through the admin API and can be re-run.

```sh
PROXCLOUD_HOST=<host> deploy/seed/seed-operator.py --provider "<provider name>"
```

The documents come from `deploy/seed/` (`platform-configuration.json`, `flavors.json`,
`billing-configuration.json`, `price-plan-seed.json`). Change them there or in the admin
console afterwards. `--help` lists the environment variables.

`deploy/seed/external-service-dev.json` holds example documents for the
`kamaji` and `dbaas` provider kinds (insert each element as its own row,
replacing the `__…__` placeholders); they need a prepared management cluster, see
[managed-k8s.md](managed-k8s.md). Its first element is a leftover example for a
provider kind that no longer exists, and there is no Proxmox example in it yet.

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
- Chart: `make deploy` runs `helm upgrade --install` with `api.image.tag=$(TAG)` (see
  [ADR-0005](adr/0005-self-contained-helm-chart.md)). `TAG` defaults to
  `dev-<7-char sha of HEAD>`, the tag CI gives a main commit, but CI publishes it only
  when that commit changed an image's sources (a docs- or chart-only commit has none),
  and there is no release tag yet to default to. Otherwise run `make image push` first,
  or pass a published `TAG=`.

## Where things live

- `cmd/api` — the server entrypoint and all wiring. `cmd/sandbox-proxy` (the public
  entry of sandbox ports, and the registry sync) and `cmd/sandbox-init` (the
  in-sandbox init) are the sandbox binaries; `cmd/e2edrv`, `cmd/e2egpu` and
  `cmd/e2eseed` are end-to-end drivers for a lab.
- `internal/platform/*` — the business domains (org, project, billing, pricing,
  payment, admin, audit, job, promotion, mcp, sso, …), each as handler/service/repo,
  plus the billing provider seam (`billingprovider`, `billingclient`,
  `billingcallback`), `pveflavor` (the Proxmox flavor catalog) and `paging`.
- `internal/cloud` — the Proxmox VE client and services (`pve`), resource cache,
  providers and sync, metrics, the ceph-s3 client, managed K8s/DBaaS, `sandbox`
  (the sandbox cluster client), `csigw` (the CSI gateway), `truenas` and `powerdns`.
- `internal/sandboxproxy` — sandbox-proxy's routing, wake and certificate sync.
- `internal/{config,server,health,pgdoc,amqp,oidc}` — wiring and infrastructure.
- `pkg/{auth,httpx,money,audit,textcrypt,billingapi,certfile,safeurl}` — cross-cutting:
  auth middleware, the response envelope + request context, money⇄decimal string,
  the audit helper, field-level encryption, the billing provider contract, a
  certificate file that reloads, and URL checks for addresses tenants choose.
- `deploy/` — `Dockerfile`, the Helm charts (`charts/proxcloud`, `charts/database-cluster`,
  `charts/proxmox-kamaji-cluster`), values overlays, the manifests for the
  management, DBaaS and sandbox clusters (`mgmt-cluster/`, `dbaas-cluster/`, `sandbox-cluster/`),
  the sandbox image Dockerfiles, and `seed/`.
- `web/client`, `web/admin` — the two React SPAs.
- `test/integration` — the testcontainers suite (`-tags=integration`).
