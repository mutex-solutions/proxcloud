# Contributing to ProxCloud

Thanks for your interest in improving ProxCloud. This guide covers local setup,
the build/test commands, and how changes flow from a branch to `main`.

By participating you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Prerequisites

- **Go 1.26.0 or newer** for the backend (the `go.mod` minimum). CI and the image
  builders use exactly Go 1.27.4; see the
  [supported Go build baseline](docs/supported-go-build-baseline.md).
- **Node.js 24** and npm — matches hosted frontend CI for both React consoles.
  The locked Vite toolchain requires at least Node 20.19 or 22.12 on those older
  release lines; arbitrary Node 20.x is not enough. The accessibility specs of the
  customer console (`@axe-core/playwright` 4.14) declare Node 22.12 or newer.
- **Docker** — required for the integration tests (they spin up a throwaway
  PostgreSQL via testcontainers) and for building images / running the local stack.
- Optional for a full run: an **OpenID Connect** issuer (Keycloak or other) and
  a **Proxmox VE** cluster. Auth-gated and cloud routes stay dark without them.

## Getting the app running locally

The fastest path is Docker Compose, which brings up the API, both SPAs,
PostgreSQL and a development Keycloak realm. RabbitMQ is absent by default and is
only retained behind the explicit `legacy-rabbitmq` migration profile:

```sh
docker compose up --build
```

- API: `http://localhost:8080` (management port `http://localhost:8081`)
- Customer console: `http://localhost:8082`
- Operator console: `http://localhost:8083`

The bundled realm supplies development logins (`demo`/`demo` and `admin`/`admin`).
Compose's OIDC/URL overrides are documented in `docker-compose.yml`; put local
values in an uncommitted `.env` file. Register PVE/S3 providers in the operator
console rather than assuming `.env` provisions them. Default credentials, plain
HTTP and open development ports are not production settings.

Scheduler/debug triggers are off by default, while primary PostgreSQL workers
continue accepted jobs. Existing deployments must follow the
[jobs migration runbook](docs/postgres-jobs-migration.md): never discard broker
volumes/backlogs with `down -v` or `--remove-orphans` as an upgrade shortcut.

### Running pieces individually

Backend:

```sh
go run ./cmd/api
```

Config is read from `application.yml` (if present at
`/opt/proxcloud/api/application.yml`) with environment-variable overlays — see
`internal/config/config.go` for the full contract.

Frontends:

```sh
cd web/client   # or web/admin
npm ci
npm run dev
```

## Build, test, and lint

Backend (via the `Makefile`):

```sh
make build             # go build ./...
make vet               # go vet ./...
make test              # go test ./...
make test-integration  # go test -tags=integration ./test/integration/... (needs Docker)
make binary            # static binary -> bin/proxcloud-api
make image             # docker build -f deploy/Dockerfile
```

Before opening a PR, make sure the code is formatted and vetted:

```sh
gofmt -l .   # should print nothing
go vet ./...
go test ./...
```

Frontends:

```sh
npm run lint    # oxlint
npm run build   # tsc type-check + production build
```

### Integration tests

Integration tests live in `test/integration` behind the `integration` build tag,
so `go test ./...` skips them by default. Run them explicitly with `make
test-integration` (or `go test -tags=integration ./test/integration/...`). They
require Docker to be running; each run gets a fresh, disposable PostgreSQL.

#### The guest images of the load balancer tests

The real-guest load balancer tests (`test/integration/lb_*_test.go`) run privileged
systemd containers of `debian:trixie` and `ubuntu:26.04`, with the distribution's own
HAProxy, keepalived and nftables. Without any setup each of them installs its packages
from the distribution's mirror while it runs, which works and is slow, and fails when
the mirror does. CI builds two images that already hold the packages
(`test/integration/images`) and names them to the tests. To do the same locally:

```sh
docker buildx build --load -t proxcloud-lb-test:debian-trixie \
  -f test/integration/images/debian-trixie.Dockerfile test/integration/images
docker buildx build --load -t proxcloud-lb-test:ubuntu-26.04 \
  -f test/integration/images/ubuntu-26.04.Dockerfile test/integration/images
export LB_TEST_IMAGE_DEBIAN=proxcloud-lb-test:debian-trixie
export LB_TEST_IMAGE_UBUNTU=proxcloud-lb-test:ubuntu-26.04
make test-integration
```

- The packages are the ones the distribution ships on the day of the build; nothing is
  pinned to a version. To take the current ones, build again with
  `--build-arg REFRESH=$(date -u +%G-W%V)` (or `--no-cache`). CI passes the ISO week as
  `REFRESH`, which is part of the package layer's cache key: the first run of a week
  installs from the mirror, a later run takes the layer from the Actions cache, which
  only a push to `main` writes.
  The base image is pinned by digest and moved by Dependabot.
- A build that cannot install says `mirror` in its last line: that is the
  distribution's mirror, not a test.
- A test that needs a package the images lack fails and says so: add the package to
  both Dockerfiles (and to the test's own `lbInstall`/`lbInit` call, which is what a
  run without the images installs).
- `TestLBApplianceServesWhatIsRecorded` never uses these images: the product's own
  install on a plain distribution image is what it shows (the pair's install line and
  the UDP service's), so it always needs the mirror.
- The UDP tests need the kernel's IPVS (`sudo modprobe ip_vs`); without it they skip,
  unless `LB_UDP_REQUIRE_IPVS` is set, as CI sets it.

## Project layout

See the [repository layout table](README.md#-repository-layout) in the README.
In short: HTTP entrypoint and job wiring in `cmd/api`, business domains in
`internal/platform/<domain>`, Proxmox VE integration in `internal/cloud`,
reusable libraries in `pkg`, deployment assets in `deploy`, and the two consoles
in `web/`.

### Adding a domain or endpoint

- A business domain is a package under `internal/platform/<domain>`, typically
  with its repository (Postgres access), a service (business logic), and HTTP
  handlers. Follow the shape of an existing domain such as `project` or
  `billing`.
- Cloud/Proxmox VE behavior belongs in `internal/cloud` (`pve`, providers,
  sync, metrics).
- Wire new routes into the appropriate surface in `cmd/api/main.go`:
  - `/api/v1` — the customer API,
  - `/admin-api/v1` — the operator API (SigV4-signed),
  - `/mcp` — the Model Context Protocol server.
- Add or extend tests. Prefer a fast unit test next to the code; use
  `test/integration` when behavior needs a real PostgreSQL.

## Branch & PR flow

1. Branch off `main` (e.g. `feat/savings-plan-rollover`, `fix/invoice-rounding`).
2. Keep commits focused. Commit subjects follow a
   [Conventional Commits](https://www.conventionalcommits.org/) style
   (`feat:`, `fix:`, `chore:`, `docs:`, …).
3. Ensure `gofmt`, `go vet`, `go test`, and (for UI changes) `npm run lint` and
   `npm run build` all pass.
4. Open a pull request against `main` and fill in the
   [PR template](.github/pull_request_template.md). Chart-path PRs run Helm checks;
   version bumps are a review requirement, not an enforced universal CI gate.
   Releases are cut by tags, not commit prefixes: a `v*` tag publishes five images
   and the app chart at the tag's version in separate workflows;
   `database-cluster-v*` / `proxmox-kamaji-cluster-v*` / `proxcloud-platform-v*` / `proxcloud-sandbox-v*` publish those charts and must
   match their `Chart.yaml` versions. No chart is published merely by merging.
   See [CI/release operations](docs/ci-release.md) and
   [chart publishing](deploy/charts/README.md). Do not create a tag as part of a
   routine documentation or code PR.
5. Address review feedback; a maintainer merges once CI is green.

## Reporting bugs & requesting features

Use the [issue templates](.github/ISSUE_TEMPLATE). For anything security-related,
please follow [`SECURITY.md`](SECURITY.md): a minimal public contact request is
allowed, but never put the full report, exploit steps or sensitive data in an issue.
