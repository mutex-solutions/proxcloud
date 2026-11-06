# proxcloud

Helm chart for the ProxCloud platform: the `proxcloud-api` Go service, the
customer console (`ui`) and the operator console (`admin`), a CloudNativePG
`Cluster`, a `RabbitmqCluster`, and optionally Keycloak (codecentric/keycloakx,
running the official `quay.io/keycloak/keycloak` image).

## Install

[`make install`](../../../docs/install.md) deploys this chart with everything it
needs. By hand, the CloudNativePG and RabbitMQ Cluster operators must already run in
the cluster:

```sh
helm dependency build deploy/charts/proxcloud
helm install proxcloud deploy/charts/proxcloud -n proxcloud --create-namespace \
  --set api.encryptionKey=$(openssl rand -hex 32) \
  --set postgresql.auth.password=$(openssl rand -hex 16)
```

Defaults deploy api + ui + admin with operator-managed PostgreSQL and RabbitMQ.
Everything else (Keycloak, Ingress, Gateway-API routes, scheduled jobs) is
off until you enable it. For a cluster without the operators,
`-f deploy/charts/proxcloud/values-dev.yaml` selects the bitnami subcharts instead.

The chart ships **no default secrets** — `api.encryptionKey` and the
PostgreSQL owner password are required (a template `fail` blocks an
install that leaves them empty, so a known key/password never reaches a live
Secret; use `api.existingSecret` / `external*.existingSecret` to supply your
own). For a throwaway local dev install, pass the insecure sample values with
`-f deploy/charts/proxcloud/values-dev.yaml`.

A fresh database needs the base configuration documents seeded before the
billing/project endpoints work — see `deploy/seed/` in the repo.

Proxmox VE clusters (and Ceph RGW stores) are not chart values: register them
after install in the operator console (System → Cloud providers). See
`docs/cloud-integration.md` and `docs/proxmox-permissions.md`.

Registering a **kamaji managed-Kubernetes provider** has an out-of-chart
prerequisite: the Kamaji management cluster must run ArgoCD and be prepared
with the operator-applied manifests in `deploy/mgmt-cluster/` (proxcloud
ServiceAccount + RBAC, the `proxcloud-k8s` AppProject, custom health checks)
before the provider is added. See `docs/managed-k8s.md`.

## Layout conventions

- Templates are grouped by component directory (`templates/api`,
  `templates/web`, `templates/gateway`, `templates/keycloak`); the two SPAs
  render from one ranged template since they share a shape.
- Every object is named `<fullname>-<component>` and carries the standard
  `app.kubernetes.io/*` labels with `component` set accordingly.
- Helpers are minimal: name/fullname/labels/selector plus the
  `PROXCLOUD_DB_URL` DSN builder and the bundled-vs-external host resolvers.

## Datastores

**PostgreSQL** (primary, required). Three backends:

- **CloudNativePG** (default) — runs Postgres via the
  [CloudNativePG](https://cloudnative-pg.io/) operator on the **official**
  `ghcr.io/cloudnative-pg/postgresql` image. Two modes: `installOperator=false` when the
  operator is already in the cluster (the chart renders just the `Cluster` CR + its
  credentials Secret), or `installOperator=true` to also bundle the operator + CRDs.
  db/owner/password come from `postgresql.auth.*`; the api connects to the
  `<cluster>-rw` service. `cnpg.keycloak` adds a role and database for the bundled Keycloak.
- **Bundled bitnami subchart** (`postgresql.enabled=true`, `cnpg.enabled=false`).
- **Managed / external** — both off + `externalPostgresql.*`.

Either way the api consumes a single DSN (`PROXCLOUD_DB_URL`): the chart builds it
into its own Secret, or reads it whole from `api.existingSecret` or
`externalPostgresql.existingSecret`/`secretKey`.

**RabbitMQ** (required). Same pattern: `rabbitmqCluster.*` (default, needs the RabbitMQ
Cluster Operator; the api reads the operator's `<name>-default-user` Secret), the bundled
`rabbitmq.*`, or `externalRabbitmq.*` (password inline or via `existingSecret`).

PostgreSQL is the only datastore.

## Identity

The API is a resource server for up to three OIDC realms (`auth.main`,
`auth.admin`, `auth.adminApi`); an empty issuer disables that realm. The
`ui` SPA logs in against `main`, the `admin` SPA against `admin`.

`keycloakx.enabled=true` bundles Keycloak, but issuers are never derived
automatically: tokens carry a host-derived `iss`, so set `auth.*.issuer`
to Keycloak's *public* URL.

For a one-shot bring-up enable `keycloakConfigCli`: a post-install/
post-upgrade Job derives the realms and clients from `auth.*` +
`api.selfUrls` (customer realm with user registration + email-as-username
on, PKCE clients with the right redirect URIs) and applies them
idempotently — including into a pre-existing `master`; clients it does not
manage are never deleted. Alternatively supply a realm export via
`realmImport.json` (rendered into `<fullname>-realm-import`) and wire it
into the subchart with `--import-realm` as shown in `values.yaml`. Without
either, the chart ships no baked realm — define your realms and the
`proxcloud-ui` / `proxcloud-admin` / `proxcloud-admin-api` clients yourself.

## Exposure

Per-component Ingress (`api.ingress`, `ui.ingress`, `admin.ingress`) or
Gateway-API HTTPRoutes (`gateway.enabled` + `gateway.parentRefs` + a
hostname per component). The management port (`:8081`) is cluster-internal
on purpose — never expose it.

## Key values

| Value | Default | Meaning |
|---|---|---|
| `api.image.repository` / `tag` | `ghcr.io/mutex-solutions/proxcloud-api` / appVersion | API image. |
| `api.replicas` | `1` | API replicas (scheduled jobs are fleet-locked, so >1 is safe). |
| `api.port` / `api.managementPort` | `8080` / `8081` | App / management listeners (change requires matching `applicationYml`). |
| `api.encryptionKey` | insecure dev default | Data-at-rest key (`PROXCLOUD_ENCRYPTION_DEFAULT_KEY`). |
| `api.existingSecret` | `""` | Pre-created Secret with `db-url`, `rabbitmq-password`, `encryption-key` (replaces the chart-managed one). |
| `api.selfUrls.{base,api,ui,admin}` | `""` | Public base URLs (`PROXCLOUD_SELF_*`); ui/admin also seed CORS. |
| `api.corsAllowedOrigins` | `[]` | Extra CORS origins. |
| `api.jobs.schedulerEnabled` | `false` | Start the billing/metrics crons (charges bills on a timer). |
| `api.jobs.debugTriggers` | `false` | Expose on-demand `/debug/run-*` triggers on the management port. |
| `api.jobs.rabbitFanout` | `false` | Charge cron via RabbitMQ fan-out. |
| `api.applicationYml` | `""` | Optional literal `application.yml` mounted at `/opt/proxcloud/api/application.yml`. |
| `api.extraEnv` | `[]` | Raw env passthrough (mail `PROXCLOUD_MAIL_*`, …). |
| `api.extraVolumes` / `api.extraVolumeMounts` | `[]` | Extra volumes/mounts on the api pod (e.g. mount a private CA — see Self-hosting → Custom CA). |
| `ui.image.repository` / `admin.image.repository` | `…/proxcloud-web` / `…/proxcloud-admin` | SPA images. |
| `ui.apiUrl` / `admin.apiUrl` | derived | Browser-facing API URL (defaults to `<api.selfUrls.api>/api/v1`). |
| `auth.main.{issuer,clientId}` | `""` / `proxcloud-ui` | Customer realm. |
| `auth.admin.{issuer,clientId}` | `""` / `proxcloud-admin` | Operator realm. |
| `auth.adminApi.{issuer,clientId}` | `""` / `proxcloud-admin-api` | Machine-to-machine realm. |
| `postgresql.enabled` | `true` | Bundle PostgreSQL. |
| `postgresql.auth.{username,password,database}` | `proxcloud` | Bundled role/db (created on first boot). |
| `externalPostgresql.{host,port,database,username,password,sslMode}` | — | Managed DB (when not bundled). |
| `externalPostgresql.existingSecret` / `secretKey` | `""` / `db-url` | Secret holding the complete DSN. |
| `rabbitmq.enabled` | `true` | Bundle RabbitMQ. |
| `externalRabbitmq.{host,port,username,password,existingSecret,secretKey}` | — | Managed broker. |
| `keycloak.enabled` | `false` | Bundle Keycloak (expose via `keycloak.ingress.*`). |
| `realmImport.json` | `""` | Optional realm export → ConfigMap. |
| `gateway.enabled` / `parentRefs` / `hostnames.{api,ui,admin}` | off | Gateway-API HTTPRoutes instead of Ingress. |
| `api.ingress` / `ui.ingress` / `admin.ingress` | disabled | Per-component Ingress (`enabled`, `className`, `host`, `path`, `annotations`, `tls`). |

## Upgrade / uninstall

```sh
helm upgrade proxcloud deploy/charts/proxcloud -n proxcloud -f my-values.yaml
helm uninstall proxcloud -n proxcloud   # PVCs from bundled datastores survive
```
