# proxcloud

Helm chart for the ProxCloud platform: the `proxcloud-api` Go service, the
customer console (`ui`) and the operator console (`admin`), PostgreSQL and
RabbitMQ, and optionally Keycloak (codecentric/keycloakx, running the official
`quay.io/keycloak/keycloak` image).

Three ways to install it:

| You have | Use | Datastores |
|---|---|---|
| Any Kubernetes cluster, nothing else installed | [Quick start](#quick-start-one-command) (`values-quickstart.yaml`) | bitnami PostgreSQL and RabbitMQ subcharts, bundled Keycloak |
| A cluster running the CloudNativePG and RabbitMQ Cluster operators | [Production install](#production-install) (`values.yaml` defaults) | operator-managed `Cluster` and `RabbitmqCluster` |
| Proxmox VE hosts and no cluster yet | [`make install`](../../../docs/install.md), which builds the cluster and deploys this chart | operators |

Docker Compose (`docker compose up` at the repo root) is the fourth option, for a
single development machine.

## Quick start (one command)

Brings up api, ui, admin, PostgreSQL, RabbitMQ and Keycloak with its realms on a
fresh cluster (kind, k3d, minikube or a real one) with no operators. Needs
`helm` 3.8+, `kubectl` and a default StorageClass.

```sh
helm dependency build deploy/charts/proxcloud
helm install proxcloud deploy/charts/proxcloud -n proxcloud --create-namespace --wait --timeout 15m \
  -f deploy/charts/proxcloud/values-quickstart.yaml \
  --set api.encryptionKey=$(openssl rand -hex 32) \
  --set postgresql.auth.password=$(openssl rand -hex 16) \
  --set rabbitmq.auth.password=$(openssl rand -hex 16) \
  --set keycloakConfigCli.adminPassword=$(openssl rand -hex 12)
```

No secret has a default: the four `--set` flags generate them, and
`helm get values proxcloud -n proxcloud` reads them back. On a laptop the install
takes about three minutes. The API restarts a few times while PostgreSQL starts;
that is expected.

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
([`docs/development.md`](../../../docs/development.md#seed-an-operator-setup)).

Check health on the management port:

```sh
kubectl -n proxcloud port-forward svc/proxcloud-api 8181:8081 &
curl localhost:8181/actuator/health/readiness   # {"status":"UP"} with postgres and rabbit UP
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

The defaults expect the [CloudNativePG](https://cloudnative-pg.io/) and
[RabbitMQ Cluster](https://www.rabbitmq.com/kubernetes/operator/operator-overview)
operators to run in the cluster already:

```sh
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
| `api.existingSecret` | `""` | Your Secret with `db-url`, `encryption-key`, and `rabbitmq-password` / `sso-client-secret` when used; replaces the chart's. |
| `api.selfUrls.{base,api,ui,admin}` | `""` | Public URLs (`PROXCLOUD_SELF_*`): mail links, redirects, MCP metadata; ui and admin also form the CORS list. |
| `api.corsAllowedOrigins` | `[]` | Extra CORS origins. |
| `api.defaultRegion` | `""` (`default`) | Region assumed when a provider names none. |
| `api.jobs.schedulerEnabled` | `false` | Start the cron jobs (charges bills on a timer). Turn on for production. |
| `api.jobs.debugTriggers` | `false` | On-demand `POST :8081/debug/run-*` triggers. Keep off in production. |
| `api.jobs.rabbitFanout` | `false` | Spread the native charge run across api pods through RabbitMQ. |
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
| `cnpg.{enabled,installOperator,instances,storage.*,existingSecret}` | on, operator present | CloudNativePG `Cluster`. |
| `postgresql.enabled` | `false` | bitnami PostgreSQL subchart instead. |
| `postgresql.auth.{username,password,database}` | `proxcloud`, — (required), `proxcloud` | Database owner, for CloudNativePG and bitnami alike. |
| `externalPostgresql.*` | — | Your own PostgreSQL (`host`, … or `existingSecret` with the full DSN). |
| `rabbitmqCluster.{enabled,replicas,storage.*}` | on | `RabbitmqCluster` (operator). |
| `rabbitmq.enabled`, `rabbitmq.auth.password` | `false`, — | bitnami RabbitMQ subchart instead. |
| `externalRabbitmq.*` | — | Your own broker. |
| `keycloakx.enabled` | `false` | Bundle Keycloak; see the subchart's values for `ingress`, `database`, `command`. |
| `keycloakConfigCli.{enabled,url,adminUser,adminPassword,existingSecret}` | off | Job that creates the realms and console clients from `auth.*` and `api.selfUrls`. |
| `seed.enabled`, `seed.platformConfiguration`, `seed.billingConfiguration` | on | First-run configuration documents (below). |

Everything else is in `values.yaml` with comments.

## Datastores

**PostgreSQL** (required): a CloudNativePG `Cluster` (default; set
`cnpg.installOperator=true` to bundle the operator too, but on a fresh cluster its
webhook may not be ready for the first apply, so re-run the install), the
bitnami subchart (`postgresql.enabled=true`, `cnpg.enabled=false`), or
`externalPostgresql.*`. The api reads one DSN (`PROXCLOUD_DB_URL`) that the chart
builds into its Secret or takes whole from `api.existingSecret` /
`externalPostgresql.existingSecret`. `cnpg.keycloak` adds a role and database
for the bundled Keycloak.

**RabbitMQ** (required): `rabbitmqCluster.*` (default; the api reads the
operator's `<name>-default-user` Secret), the bitnami subchart (`rabbitmq.*`), or
`externalRabbitmq.*`.

The bitnami subcharts are a convenience for small installs: Bitnami has moved
its free images, so RabbitMQ pulls from the frozen `bitnamilegacy` mirror and
PostgreSQL from the moving `bitnami/postgresql:latest`. Pin them to your own
registry or use the operators for anything that lasts.

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

```sh
helm dependency build deploy/charts/proxcloud
helm upgrade proxcloud deploy/charts/proxcloud -n proxcloud --reuse-values --wait
```

`--reuse-values` keeps the generated secrets of a quick start; with a values
file, pass the same file and secrets again (`helm get values proxcloud -n proxcloud`
shows them). The keycloak-config-cli and seed Jobs run again and change nothing
that exists. Port-forwards end when the pods they point at restart: start them
again after an upgrade.

## Uninstalling

```sh
helm uninstall proxcloud -n proxcloud
kubectl -n proxcloud delete pvc --all   # the database and broker volumes outlive the release
kubectl delete namespace proxcloud
```

Delete the PVCs only when you want the data gone. Reinstalling over kept volumes
needs the same `postgresql.auth.password` and `api.encryptionKey` as before.

## Layout conventions

- Templates are grouped by component directory (`templates/api`, `templates/web`,
  `templates/gateway`, `templates/keycloak`, `templates/cnpg`, `templates/rabbitmq`);
  the two SPAs render from one ranged template.
- Every object is named `<fullname>-<component>` and carries the standard
  `app.kubernetes.io/*` labels with `component` set accordingly.
- Kamaji managed Kubernetes needs a prepared management cluster outside this
  chart (`deploy/mgmt-cluster/`, [`docs/managed-k8s.md`](../../../docs/managed-k8s.md)).
