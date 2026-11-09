# Installing on Kubernetes

ProxCloud is delivered as one Helm chart that installs the three application workloads and, by default, every stateful dependency they rely on. A single `helm upgrade --install` yields a complete platform: API, customer portal, admin console, a PostgreSQL cluster and a RabbitMQ cluster (through their operators), plus, optionally, a bundled Keycloak for identity.

## Prerequisites

- A working Kubernetes cluster with a default StorageClass.
- The [CloudNativePG](https://cloudnative-pg.io) operator and the [RabbitMQ Cluster Operator](https://github.com/rabbitmq/cluster-operator), which run the default PostgreSQL and RabbitMQ. Without them, set `cnpg.installOperator: true` (bundles the CloudNativePG operator), use the bitnami subcharts (`-f deploy/charts/proxcloud/values-dev.yaml`), or point at external services (below).
- For classic ingress: an ingress controller (such as ingress-nginx) and, for TLS, cert-manager with a ClusterIssuer. Gateway API exposure is supported as an alternative.
- DNS names for the portal, the API and the admin console (e.g. `cloud.example.com`, `api.cloud.example.com`, `admin.cloud.example.com`) pointed at your ingress.
- Helm 3.8+.

## The chart and its images

| | |
|---|---|
| Chart | `oci://ghcr.io/mutex-solutions/charts/proxcloud` |
| API image | `ghcr.io/mutex-solutions/proxcloud-api` (Go backend: REST + admin API + jobs) |
| Portal image | `ghcr.io/mutex-solutions/proxcloud-web` (customer SPA, served at `/`) |
| Admin image | `ghcr.io/mutex-solutions/proxcloud-admin` (admin SPA, served at `/` on its own host) |

Bundled dependencies, each swappable for an external instance:

| Dependency | Toggle | External counterpart |
|------------|--------|----------------------|
| PostgreSQL | `cnpg.enabled` (default: a CloudNativePG `Cluster`), or `postgresql.enabled` (bitnami subchart) | `externalPostgresql.*`, with both off |
| RabbitMQ | `rabbitmqCluster.enabled` (default: a `RabbitmqCluster`), or `rabbitmq.enabled` (bitnami subchart) | `externalRabbitmq.*`, with both off |
| Keycloak | `keycloakx.enabled` (off by default) | any external IdP via `auth.*.issuer` (see [How Identity Works](/docs/concepts/identity)) |

The application state lives in one PostgreSQL database (`proxcloud`), one document-per-aggregate in a `jsonb` column. The bundled PostgreSQL (the CloudNativePG cluster, or the subchart) creates the `proxcloud` role and database on first boot, named by `postgresql.auth.*`; set `postgresql.auth.password` (the chart ships no default).

## A minimal values file

```yaml
# values.yaml
api:
  # Data-at-rest encryption key — CHANGE THIS and keep it safe (see Backup).
  encryptionKey: "<a long random string>"
  # Mail gateway (optional here — you can also configure SMTP later under
  # Admin → Integrations → Mail, which is stored in the database and wins).
  mail:
    from: support@example.com
    smtp:
      host: smtp.example.com
      port: 587
      username: mailer@example.com
      existingSecret: my-smtp-secret        # holds the SMTP password / API key
      passwordKey: PROXCLOUD_MAIL_SMTP_PASSWORD
  # Every component gets its own host: the consoles are built for /, and the
  # API serves more than /api (/admin-api/v1, /mcp, /e2b, /.well-known).
  # selfUrls are what the API puts in mail links, redirects and MCP metadata.
  selfUrls:
    base: "https://cloud.example.com"
    api: "https://api.cloud.example.com"
    ui: "https://cloud.example.com"
    admin: "https://admin.cloud.example.com"
  ingress:
    enabled: true
    className: "nginx"
    host: "api.cloud.example.com"
    path: /
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    tls:
      - hosts: ["api.cloud.example.com"]
        secretName: proxcloud-api-tls

ui:
  ingress:
    enabled: true
    className: "nginx"
    host: "cloud.example.com"
    path: /
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    tls:
      - hosts: ["cloud.example.com"]
        secretName: proxcloud-tls

admin:
  ingress:
    enabled: true
    className: "nginx"
    host: "admin.cloud.example.com"
    path: /
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    tls:
      - hosts: ["admin.cloud.example.com"]
        secretName: proxcloud-admin-tls

# The PostgreSQL owner's password (CloudNativePG reads it from here too).
postgresql:
  auth:
    password: "<a strong password>"
```

With ingress on, the API answers on `api.cloud.example.com` (`/api/v1`, `/admin-api/v1`, `/mcp`, `/e2b`, `/.well-known`), the portal on `cloud.example.com` and the admin console on `admin.cloud.example.com`; the consoles call the API at `api.selfUrls.api` + `/api/v1`. A bundled Keycloak (if enabled) is exposed on its own host via `keycloakx.ingress.*`.

## Running the install

```sh
helm upgrade --install proxcloud oci://ghcr.io/mutex-solutions/charts/proxcloud \
  --namespace proxcloud --create-namespace \
  -f values.yaml
```

Watch it come up:

```sh
kubectl -n proxcloud get pods
kubectl -n proxcloud get ingress
```

The API connects to PostgreSQL before it listens and exits if it cannot, so expect a few restarts on first boot while the database comes up.

To pin application versions explicitly (otherwise the chart's `appVersion` is used):

```sh
--set api.image.tag=<TAG> --set ui.image.tag=<TAG> --set admin.image.tag=<TAG>
```

## Exposing via Gateway API instead of Ingress

For a cluster on the Gateway API, turn classic ingress off and attach HTTPRoutes to your Gateway:

```yaml
api:
  selfUrls:
    base: "https://cloud.example.com"   # public URL, since it can't be derived from ingress
gateway:
  enabled: true
  parentRefs:
    - name: my-gateway
      namespace: gateways
      sectionName: https
  hostnames:
    api: api.cloud.example.com
    ui: cloud.example.com
    admin: admin.cloud.example.com
```

Leave the per-component `*.ingress.enabled` off (the default) and set `api.selfUrls.base` (and optionally `api.selfUrls.api`/`ui`/`admin`) so the API knows its public URLs.

## The encryption key — don't skip this

The API encrypts sensitive fields at rest with `PROXCLOUD_ENCRYPTION_DEFAULT_KEY`. The chart holds it in the API Secret **`<release>-api`** under the key `encryption-key`, sourced from `api.encryptionKey` (or your own `api.existingSecret`). Set a strong value and store it somewhere safe the moment the first install finishes:

```sh
kubectl -n proxcloud get secret proxcloud-api \
  -o jsonpath='{.data.encryption-key}' | base64 -d
```

Lose this value and encrypted data cannot be recovered — see [Backup and Recovery](/docs/self-hosting/backup). To supply your own secret instead of the chart-managed one, set `api.existingSecret` to a Secret carrying the keys `db-url`, `rabbitmq-password`, and `encryption-key`.

## Pointing at external dependencies

For example, using an existing PostgreSQL rather than the bundled one:

```yaml
cnpg:
  enabled: false
postgresql:
  enabled: false
externalPostgresql:
  host: pg.example.com
  port: 5432
  database: proxcloud
  username: proxcloud
  password: "…"
  sslMode: require
  # Or, instead of the fields above, a pre-created Secret whose key holds the
  # COMPLETE DSN (postgres://user:pass@host:port/db?sslmode=...):
  # existingSecret: my-pg-secret
  # secretKey: db-url
```

`externalRabbitmq` follows the same shape (`rabbitmqCluster.enabled: false` plus the `externalRabbitmq` block). For identity, point `auth.*.issuer` at your IdP — see [How Identity Works](/docs/concepts/identity).

## Scaling for production

```yaml
api:
  replicas: 3
ui:
  replicas: 3
admin:
  replicas: 3
```

For the datastore, either give the CloudNativePG cluster replicas (`cnpg.instances: 3`) or point `externalPostgresql.*` at a managed/HA PostgreSQL that has its own backup and failover.

Scale each workload manually with `api.replicas` / `ui.replicas` / `admin.replicas`; the chart ships no HorizontalPodAutoscaler.

## Once it's installed

1. Save the `<release>-api` encryption key offline (as above).
2. Open `https://admin.cloud.example.com/` and sign in with an admin-realm account.

3. Add your Proxmox VE cluster(s) and API token in the admin console. ProxCloud syncs resource state by polling the PVE API, so there is no event feed to set up.
4. If you enable per-organization SSO, restrict Keycloak's egress first: tenants choose URLs Keycloak will call. See [Required: Keycloak egress policy](/docs/self-hosting/sso#required-keycloak-egress-policy).

## Upgrading

Re-run the same command with the new chart/app version:

```sh
helm -n proxcloud upgrade proxcloud oci://ghcr.io/mutex-solutions/charts/proxcloud -f values.yaml
```
