# proxcloud

Helm chart for the ProxCloud platform: the `proxcloud-api` Go service, the
customer console (`ui`) and the operator console (`admin`), PostgreSQL-backed
durable jobs (no broker), and optionally Keycloak (codecentric/keycloakx, running the official
`quay.io/keycloak/keycloak` image).

Three ways to install it:

| You have | Use | Datastores |
|---|---|---|
| Any Kubernetes cluster, nothing else installed | [Quick start](#quick-start-one-command) (`values-quickstart.yaml`) | bitnami PostgreSQL, bundled Keycloak; no broker |
| A cluster running CloudNativePG | [Production install](#production-install) (`values.yaml` defaults) | operator-managed PostgreSQL `Cluster`; no RabbitMQ/operator |
| Proxmox VE hosts and no cluster yet | [`make install`](../../../docs/install.md), which builds the cluster and deploys this chart | operators |

Docker Compose (`docker compose up` at the repo root) is the fourth option, for a
single development machine.

**Release status (2026-11-14):** the chart/BOM selects `v0.5.2`, the current stable
release (after the first stable `v0.4.0`; it has breaking API changes, see the
changelog). Its tag publishes this chart version
and the images; verify the actual public chart and image artifacts before
installation. Public development container tags are not a versioned release. A
clean install from the published artifacts and live installer/split-mode
acceptance have not been performed for this release. Use the source-build path
below or explicitly pin a compatible verified set of images; do not treat a
successful render/CI run as production acceptance.

## Quick start (one command)

Brings up api, ui, admin, PostgreSQL durable jobs and Keycloak with its realms on a
fresh cluster (kind, k3d, minikube or a real one) with no operators. Needs
`helm` 3.8+, `kubectl` and a default StorageClass.

**Images first.** The chart pulls `ghcr.io/mutex-solutions/proxcloud-{api,web,admin}`
at the chart's `appVersion` (`v0.5.2`). **Verify the release's images are
published and pullable before using these pull defaults.** Otherwise use images
built from the checkout containing the PostgreSQL job engine, or explicitly pin a
known compatible new image/tag/digest. Do not point the new chart at a v0.3.x API:
it requires RabbitMQ and cannot use these defaults. Until the release images are
verified pullable, build the three images from the core-containing checkout and
load them into the cluster:

```sh
docker build -t proxcloud-api:local -f deploy/Dockerfile .
docker build -t proxcloud-web:local --build-arg APP=client web
docker build -t proxcloud-admin:local --build-arg APP=admin web
kind load docker-image proxcloud-api:local proxcloud-web:local proxcloud-admin:local
# k3d: k3d image import <same three>; minikube: minikube image load <each>;
# a real cluster: push them to a registry it can pull from and use that name below.
```

and add this flag to the `helm install` below:

```sh
--set api.image.repository=proxcloud-api,api.image.tag=local,ui.image.repository=proxcloud-web,ui.image.tag=local,admin.image.repository=proxcloud-admin,admin.image.tag=local
```

Without it, or a published release, the pods stay in `ImagePullBackOff`.
Then install:

```sh
helm repo add codecentric https://codecentric.github.io/helm-charts
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm dependency build deploy/charts/proxcloud
helm install proxcloud deploy/charts/proxcloud -n proxcloud --create-namespace --wait --timeout 15m \
  -f deploy/charts/proxcloud/values-quickstart.yaml \
  --set api.encryptionKey=$(openssl rand -hex 32) \
  --set postgresql.auth.password=$(openssl rand -hex 16) \
  --set keycloakConfigCli.adminPassword=$(openssl rand -hex 12)
```

No secret has a default: the three `--set` flags generate them, and
`helm get values proxcloud -n proxcloud` reads them back. Treat that output and Helm
release metadata as sensitive; for a long-lived site use pre-created Secrets as
below. Install time depends on image downloads, storage and database/Keycloak
startup; the API can restart while dependencies become ready. Do not use a fixed
startup time as an acceptance check.

When it finishes, the release notes print the port-forwards. Run them and keep
them running:

```sh
kubectl -n proxcloud port-forward svc/proxcloud-api 8180:8080 &
kubectl -n proxcloud port-forward svc/proxcloud-ui 8182:80 &
kubectl -n proxcloud port-forward svc/proxcloud-admin 8183:80 &
kubectl -n proxcloud port-forward svc/proxcloud-keycloak-http 8190:80 &
```

| Open | Sign in with |
|---|---|
| Customer console http://localhost:8182 | Register an account on the sign-in page (realm `proxcloud`). |
| Operator console http://localhost:8183 | `admin` and the password from `kubectl -n proxcloud get secret proxcloud-keycloak-admin -o jsonpath='{.data.admin-password}' \| base64 -d` |
| Keycloak http://localhost:8190 | The same `admin`. |

These URLs are written into the tokens and the consoles, so keep the ports. The
seed Job has already written the platform and billing configuration, so a new
customer can create an organization and a project straight away. Next, register
a Proxmox VE cluster in the operator console (System → Cloud providers, see
[`docs/proxmox-permissions.md`](../../../docs/proxmox-permissions.md)) and run
`deploy/seed/seed-operator.py` for flavors, a price plan and the default quota
([`docs/development.md`](../../../docs/development.md#seed-an-operator-setup)),
pointed at the port-forwards:

```sh
PROXCLOUD_API=http://localhost:8180/api/v1 \
PROXCLOUD_TOKEN_URL=http://localhost:8190/realms/master/protocol/openid-connect/token \
PROXCLOUD_ADMIN_PASSWORD="$(kubectl -n proxcloud get secret proxcloud-keycloak-admin -o jsonpath='{.data.admin-password}' | base64 -d)" \
deploy/seed/seed-operator.py --provider "<provider name>"
```

Check health on the management port:

```sh
kubectl -n proxcloud port-forward svc/proxcloud-api 8181:8081 &
curl localhost:8181/actuator/health/readiness   # readiness uses PostgreSQL; no RabbitMQ dependency
```

The quick start is not for production: one replica of everything, plain HTTP,
and Keycloak's own administrator doubles as the platform operator.

### Expose the quick start with an Ingress

To reach it from other machines instead of port-forwards, give each component a
host on your ingress controller and change every `localhost` URL to match,
for example with `example.org`:

```yaml
# my-values.yaml, used with -f values-quickstart.yaml -f my-values.yaml
api:
  selfUrls: {base: https://app.example.org, api: https://api.example.org, ui: https://app.example.org, admin: https://admin.example.org}
  ingress: {enabled: true, className: nginx, host: api.example.org}
ui:
  ingress: {enabled: true, className: nginx, host: app.example.org}
admin:
  ingress: {enabled: true, className: nginx, host: admin.example.org}
auth:
  main: {issuer: https://id.example.org/realms/proxcloud}
  admin: {issuer: https://id.example.org/realms/master}
  adminApi: {issuer: https://id.example.org/realms/master}
keycloakx:
  command: [/opt/keycloak/bin/kc.sh, start, --http-enabled=true, --http-port=8080,
            --proxy-headers=xforwarded, --hostname=https://id.example.org, --hostname-backchannel-dynamic=true]
  ingress:
    enabled: true
    ingressClassName: nginx
    rules: [{host: id.example.org, paths: [{path: /, pathType: Prefix}]}]
```

Add `tls` blocks for HTTPS. On plain HTTP to a host other than `localhost`, set
`keycloakConfigCli.sslRequired: none` and `keycloakConfigCli.requirePkce: false`:
a browser has no WebCrypto outside a secure context, so PKCE cannot work there.

## Production install

This section describes production-oriented configuration, not a certification of
the release or a clean public-artifact install. Plan TLS, backups and
restore validation, OIDC access boundaries, resource capacity and observability
for your site; the HTTP/one-replica quickstart is not production configuration.
The defaults expect the [CloudNativePG](https://cloudnative-pg.io/) operator
to run in the cluster already. No RabbitMQ cluster/operator is required:

```sh
helm repo add codecentric https://codecentric.github.io/helm-charts   # once per machine
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm dependency build deploy/charts/proxcloud
helm install proxcloud deploy/charts/proxcloud -n proxcloud --create-namespace -f my-values.yaml \
  --set api.encryptionKey=$(openssl rand -hex 32) \
  --set postgresql.auth.password=$(openssl rand -hex 16)
```

`my-values.yaml` sets at least `api.selfUrls.*`, `auth.*.issuer` and the ingress
or Gateway API blocks. For a long-lived install put the secrets in your own
Secrets (`api.existingSecret`, `cnpg.existingSecret`,
`keycloakConfigCli.existingSecret`) instead of `--set`. `installer/` shows a
complete production set: `installer/playbooks/templates/app-proxcloud.yml.j2`.

## Values you set

| Value | Default | Meaning |
|---|---|---|
| `api.encryptionKey` | — (required) | Data-at-rest key (`PROXCLOUD_ENCRYPTION_DEFAULT_KEY`). Never change it after install: stored secrets become unreadable. |
| `api.existingSecret` | `""` | Your Secret with writable-primary `db-url`, `encryption-key` and `sso-client-secret` when SSO is enabled; optional `db-read-url` is history/metrics only. No broker key required. Old Secrets missing the read key still work. |
| `api.selfUrls.{base,api,ui,admin}` | `""` | Public URLs (`PROXCLOUD_SELF_*`): mail links, redirects, MCP metadata; ui and admin also form the CORS list. |
| `api.corsAllowedOrigins` | `[]` | Extra CORS origins. |
| `api.defaultRegion` | `""` (`default`) | Region assumed when a provider names none. |
| `api.jobs.schedulerEnabled` | `false` | Admit new scheduled billing/sync work. Opt in deliberately after fresh-install verification or the migration/cutover checklist; false does not stop existing durable workers. |
| `api.jobs.debugTriggers` | `false` | On-demand `POST :8081/debug/run-*` triggers. Keep off in production. |
| `api.jobs.workerConcurrency` | `2` | Bounded workers per API pod, `1..16`; always run even with scheduler/debug off. |
| `api.jobs.pollInterval`, `api.jobs.leaseDuration` | `1s`, `1m` | Go durations, `100ms..1m` / `10s..10m`. |
| `api.jobs.rabbitFanout` | `false` | Deprecated; **true is a migration error**, not a legacy consumer opt-in. |
| `jobsMigration.*` | unset | Existing upgrades require retain/cutover + stopped old producers/consumers, drain/reconciliation and snapshot evidence ([runbook](../../../docs/postgres-jobs-migration.md)). |
| `api.billing.{provider,url,token,timeout,callbackToken}` | `native` | `external` hands rating to a separate billing service ([`docs/configuration.md`](../../../docs/configuration.md)). |
| `api.mail.{from,businessName}`, `api.mail.smtp.{host,port,username,existingSecret,passwordKey}` | off | Bootstrap SMTP gateway; operators can also set mail in the console. |
| `api.csiGateway.{enabled,url,allowedCIDRs,service.*}` | off | Listener for tenant clusters' proxmox-csi controllers ([`docs/managed-k8s.md`](../../../docs/managed-k8s.md)); needs cert-manager. |
| `api.replicas` | `1` | API replicas (jobs are fleet-locked, so more than one is safe). |
| `api.image.{repository,tag}`, `ui.image.*`, `admin.image.*` | `ghcr.io/mutex-solutions/proxcloud-{api,web,admin}`, appVersion | Images. |
| `api.ingress`, `ui.ingress`, `admin.ingress` | off | Per-component Ingress (`enabled`, `className`, `host`, `path`, `annotations`, `tls`). |
| `gateway.{enabled,parentRefs,hostnames.*}` | off | Gateway API HTTPRoutes instead of Ingress. |
| `auth.main.issuer` / `auth.admin.issuer` / `auth.adminApi.issuer` | `""` | Public issuer URLs of the customer, operator and machine realms; empty turns a realm off. |
| `auth.*.discoveryUri` | `""` | Where the api fetches the OIDC metadata when the public URL is not reachable from the pod. |
| `auth.sso.{enabled,clientSecret,reservedDomains}` | off | Per-organization single sign-on. |
| `cnpg.{enabled,installOperator,instances,storage.*,existingSecret}` | on, operator present | CloudNativePG `Cluster`; API/queue/finance always connect to `-rw`. |
| `cnpg.statusReadReplica` | `false` | Opt-in status/history/metrics reader at `-ro`, requires `instances >= 2`; never worker/finance input. |
| `postgresql.enabled` | `false` | bitnami PostgreSQL subchart instead. |
| `postgresql.auth.{username,password,database}` | `proxcloud`, — (required), `proxcloud` | Database owner, for CloudNativePG and bitnami alike. |
| `externalPostgresql.*` | — | Your own PostgreSQL (`host`, … or `existingSecret` with the full DSN). |
| `externalPostgresql.readExistingSecret`, `readSecretKey` | `""`, `db-read-url` | Optional pre-created reader DSN Secret/key. No inline read password. |
| `rabbitmqCluster.{enabled,name,retainedSpec}` | **false** | Migration-only retained CR with Helm keep + Argo `Prune=false,Delete=false`; requires existing operator and explicit retain mode. |
| `rabbitmq.enabled`, `rabbitmq.auth.*` | **false** | Deprecated: retain an old subchart only; do not provision a new broker. Preserve its full old values. |
| `externalRabbitmq.*` | — | Deprecated/unused by the API; no broker credentials mounted. |
| `keycloakx.enabled` | `false` | Bundle Keycloak; see the subchart's values for `ingress`, `database`, `command`. |
| `keycloakConfigCli.{enabled,url,adminUser,adminPassword,existingSecret}` | off | Job that creates the realms and console clients from `auth.*` and `api.selfUrls`. |
| `seed.enabled`, `seed.platformConfiguration`, `seed.billingConfiguration` | on | First-run configuration documents (below). |

Everything else is in `values.yaml` with comments.

Local regression matrix (Helm, Compose config and actual Ansible migration gates
against mocked reads; no live resources): `python3 deploy/tests/test_durable_jobs.py`.
Run after fetching chart dependencies; requires Helm, Docker Compose, PyYAML,
Jinja2 and `ansible-playbook` on PATH. It covers broker-free defaults, explicit
legacy retention, optional-reader compatibility, and pre-apply refusal of invalid
worker/replica settings, unknown/RBAC errors and old Argo source formats.

## Datastores

**PostgreSQL** (required): a CloudNativePG `Cluster` (default; set
`cnpg.installOperator=true` to bundle the operator too, but on a fresh cluster its
webhook may not be ready for the first apply, so re-run the install), the
bitnami subchart (`postgresql.enabled=true`, `cnpg.enabled=false`), or
`externalPostgresql.*`. The api reads one DSN (`PROXCLOUD_DB_URL`) that the chart
builds into its Secret or takes whole from `api.existingSecret` /
`externalPostgresql.existingSecret`. `cnpg.keycloak` adds a role and database
for the bundled Keycloak.

**Optional reader**: `PROXCLOUD_DB_READ_URL` serves history/metrics reads only.
Missing/empty means primary fallback; a configured reader failure returns 503 for
those reads. `cnpg.statusReadReplica` explicitly opts into `-ro` generation, never
by default. With `api.existingSecret`, supply the optional `db-read-url` yourself;
its key reference is optional for backward compatibility. A separately configured
`externalPostgresql.readExistingSecret` key is required. Seed jobs, durable queue
operations and financial reads/writes always use `PROXCLOUD_DB_URL`.

**RabbitMQ is not an application dependency.** Broker toggles are deprecated and
default false. Retain only an **existing** broker for migration, and never delete
its historical data as part of this upgrade. See [Upgrading](#upgrading).

The bitnami PostgreSQL subchart is a convenience for small installs: its image is
moving `bitnami/postgresql:latest`. Pin your own image or use CloudNativePG for
anything that lasts. The old RabbitMQ subchart remains for explicit legacy
retention compatibility, not as a fresh-install dependency.

Scheduler and debug gates default false; durable workers still process existing
jobs. Current source provisions a dedicated mail outbox and offers separate opt-in
enqueue paths for savings reminders/expiry, project invites and approved
validations. These producer flags default off and **do not enable SMTP dispatch**;
the application does not start the durable SMTP dispatcher. Do not interpret
billing-worker readiness or an outbox row as proof of delivered mail. Existing
non-outbox mail paths and the external billing HTTP contract remain separate.

## Identity

The API is a resource server for up to three OIDC realms (`auth.main`,
`auth.admin`, `auth.adminApi`); `ui` signs in against `main`, `admin` against
`admin`. Any user who signs in to the operator realm through the operator
client is a platform operator, so keep that realm to your staff.

Issuers are never derived: tokens carry the URL the browser used, so set
`auth.*.issuer` to Keycloak's public URL and, when pods cannot reach it,
`auth.*.discoveryUri` to the in-cluster Service.

`keycloakConfigCli.enabled` runs a post-install/post-upgrade Job that creates the
customer realm (registration and email-as-username on) and the PKCE clients with
the right redirect URIs, and adds clients to an existing realm such as `master`.
It never deletes clients it does not manage. `realmImport.json` is the
alternative: your own realm export in a ConfigMap.

## First-run seed

A fresh database needs a platform and a billing configuration document before
the billing and project endpoints work. With `seed.enabled` (default) a
post-install/post-upgrade Job writes `seed.platformConfiguration` and
`seed.billingConfiguration` (the same documents as `deploy/seed/`), each only
while its table is empty. Later edits in the operator console or by
`deploy/seed/seed-operator.py` are never overwritten. Change the base currency
before the first install with `--set seed.billingConfiguration.baseCurrency=EUR`.

## Exposure

Per-component Ingress or Gateway API HTTPRoutes. The management port (`:8081`:
probes and operator triggers) stays cluster-internal; never route to it.

## Upgrading

**Read [the migration runbook](../../../docs/postgres-jobs-migration.md) first.**
Existing Helm upgrades fail rendering without explicit `jobsMigration.mode=retain`
or `cutover`, stopped producers/consumers, recorded queue drain/reconciliation and
snapshot evidence. Do not treat `rabbitFanout=false` as a drain proof. Legacy
messages lack the original billing cutoff; replaying them at today's time is unsafe.

Before omission, protect the **live** RabbitmqCluster with Helm keep and Argo
`Prune=false,Delete=false`. For Helm you must also do a **retained-release transition**:
first stage annotation-only retention with the **old chart reference/API image**,
then keep the exact old CR/name/spec enabled in new-chart retain mode after old
work is reconciled/stopped. Confirm the stored release
manifest contains `helm.sh/resource-policy: keep`, and only then omit it in cutover
mode with `retentionPrepared=true`. A live-only annotation is not sufficient for
Helm's previous-manifest comparison. ArgoCD installer upgrades require the live
prune/delete protection before stage 40 applies anything; it never auto-patches
or removes a broker/PVC. An old Bitnami subchart needs its separate manual
child-resource retention plan before omission.

```sh
helm dependency build deploy/charts/proxcloud
# migration-values.yaml: reviewed retain/cutover decision + evidence; keep old secrets.
helm template proxcloud deploy/charts/proxcloud -n proxcloud --is-upgrade -f my-values.yaml -f migration-values.yaml
helm upgrade proxcloud deploy/charts/proxcloud -n proxcloud -f my-values.yaml -f migration-values.yaml --wait
```

Do not blindly `--reuse-values`: it may carry a true fanout flag or old broker
toggle. Supply the same encryption/database keys and review saved values. Even a
broker-free existing release must explicitly record its no-legacy state for the
first upgrade. Retain migration values for later upgrades. Keycloak-config-cli and
seed Jobs preserve existing records. Restart port-forwards after pod replacement.

## Uninstalling

Uninstall is **not** a migration step. Back up data, inspect release-managed CRs
and their deletion policies, and follow a separately approved teardown plan.
Deleting a RabbitmqCluster or namespace may cascade to its children/PVCs; a keep
annotation is not protection against explicit Kubernetes deletion. Do not run a
blanket PVC delete or namespace delete as part of this upgrade. Reinstalling over
kept PostgreSQL volumes needs the same database credentials and encryption key.

## Layout conventions

- Templates are grouped by component directory (`templates/api`, `templates/web`,
  `templates/gateway`, `templates/keycloak`, `templates/cnpg`, `templates/rabbitmq`);
  the two SPAs render from one ranged template.
- Every object is named `<fullname>-<component>` and carries the standard
  `app.kubernetes.io/*` labels with `component` set accordingly.
- Kamaji managed Kubernetes needs a prepared management cluster outside this
  chart (`deploy/mgmt-cluster/`, [`docs/managed-k8s.md`](../../../docs/managed-k8s.md)).
