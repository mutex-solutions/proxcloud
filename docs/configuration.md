# Configuration

How the ProxCloud API (`proxcloud-api`) is configured, for contributors and operators.
The source of truth is `internal/config/config.go` (plus a few subsystems that read
their own env, noted below). Deployment wiring lives in `deploy/charts/proxcloud`.

## Sources and precedence

Configuration is a **config file overlaid by environment variables**, and
**env always wins over the file**.

- **Config file:** `application.yml`, loaded from `/opt/proxcloud/api/application.yml`
  by default. Override the path with `PROXCLOUD_CONFIG_FILE`. If the file is absent,
  the service runs on env + defaults alone.
- **Environment variables:** injected by the deployment (secrets, connection
  strings, toggles). Any env key listed below takes priority over its file key.

The Helm chart mounts a rendered `application.yml` (from `values.yaml`) at the
default path and injects the secret/connection env vars into the pod
(`deploy/charts/proxcloud/templates/api/configmap.yaml` and `deploy/charts/proxcloud/templates/api/deployment.yaml`).

Required config is validated at startup (`Config.Validate`): the service refuses
to start without `PROXCLOUD_DB_URL` and a RabbitMQ host.

## Configuration reference

Legend: **env** = environment variable, **file** = `application.yml` key, **helm**
= `values.yaml` path.

### Server

| Concern | env | file | helm | Default | Meaning |
|---------|-----|------|------|---------|---------|
| API port | — | `server.port` | fixed `8080` | `8080` | Public API listener. |
| Management port | — | `management.server.port` | fixed `8081` | `8081` | Health probes + operator debug triggers. |
| Log level | — | `logging.level.root` | `api.logLevel` | `INFO` | Root log level. |

### PostgreSQL (required)

| Concern | env | file | helm | Default | Meaning |
|---------|-----|------|------|---------|---------|
| Connection DSN | `PROXCLOUD_DB_URL` | `db.url` | derived from `postgresql.*` or `externalPostgresql.*` | — (required) | PostgreSQL connection string (`postgres://user:pass@host:port/proxcloud?sslmode=…`). |

PostgreSQL is the sole primary datastore: every document kind is a
`(id text primary key, doc jsonb not null)` table (see
[data-model.md](data-model.md)). The chart builds the DSN from the bundled
`postgresql` subchart or from `externalPostgresql.*` and injects it as env;
the credentials come from a secret (or `externalPostgresql.existingSecret`).
Documents are (de)serialized with the standard-library `encoding/json` (no ORM
or external codec) and stored verbatim in each table's `doc` jsonb column.

### RabbitMQ (host required)

| Concern | env | file | helm | Default | Meaning |
|---------|-----|------|------|---------|---------|
| Host | `PROXCLOUD_RABBITMQ_HOST` | `rabbitmq.host` | derived from `rabbitmq.*` / `externalRabbitmq.host` | — (required) | Broker host. |
| Port | `PROXCLOUD_RABBITMQ_PORT` | `rabbitmq.port` | derived | `5672` | Broker port. |
| Username | `PROXCLOUD_RABBITMQ_USERNAME` | `rabbitmq.username` | secret | — | Broker user. |
| Password | `PROXCLOUD_RABBITMQ_PASSWORD` | `rabbitmq.password` | secret | — | Broker password. |

### Encryption (data-at-rest key)

| Concern | env | file | helm | Default | Meaning |
|---------|-----|------|------|---------|---------|
| Default key | `PROXCLOUD_ENCRYPTION_DEFAULT_KEY` | `proxcloud.encryption.default.key` (alias `default-key`) | `api.encryptionKey` (or `api.existingSecret`) | — | Key used to encrypt sensitive fields at rest. |

The chart renders `api.encryptionKey` into a Secret and injects it as
`PROXCLOUD_ENCRYPTION_DEFAULT_KEY`. It protects provider and integration secrets and the secrets of admin hmac keys, and
changing it locks all of those out: hmac key rows that no longer decrypt fail authentication until they are minted again.
Alternatively, point `api.existingSecret` at a
pre-made Secret carrying `db-url` / `rabbitmq-password` / `encryption-key`.

### Self URLs

Used to build absolute links (mail, redirects, MCP resource metadata).

| Concern | env | file | helm | Meaning |
|---------|-----|------|------|---------|
| Base URL | `PROXCLOUD_SELF_BASE_URL` | `proxcloud.self.base-url` | `api.selfUrls.base` | Overall public base URL. |
| API base URL | `PROXCLOUD_SELF_API_BASE_URL` | `proxcloud.self.api-base-url` | `api.selfUrls.api` | Public API URL (also the MCP resource base). |
| UI base URL | `PROXCLOUD_SELF_UI_BASE_URL` | `proxcloud.self.ui-base-url` | `api.selfUrls.ui` | Customer console URL. |
| Admin base URL | `PROXCLOUD_SELF_ADMIN_BASE_URL` | `proxcloud.self.admin-base-url` | `api.selfUrls.admin` | Admin console URL. |

### OIDC realms

Three realm configs, each an issuer URI + client id. See `docs/auth.md` for the
identity model. Empty issuer = realm disabled (tokens for it are rejected).

| Realm | env (issuer / client) | file | helm |
|-------|-----------------------|------|------|
| Customer (`clients`) | `AUTH_MAIN_OAUTH2_ISSUER_URI` / `AUTH_MAIN_OAUTH2_CLIENT_ID` | `auth.main.oauth2.issuer-uri` / `.client-id` | issuer: `auth.main.issuer` (empty ⇒ bundled Keycloak `clients` realm); client: `auth.main.clientId` (`proxcloud-ui`) |
| Operator (`master`) | `AUTH_ADMIN_OAUTH2_ISSUER_URI` / `AUTH_ADMIN_OAUTH2_CLIENT_ID` | `auth.admin.oauth2.issuer-uri` / `.client-id` | issuer: `auth.admin.issuer` (empty ⇒ bundled `master` realm); client: `auth.admin.clientId` (`proxcloud-admin`) |
| Admin API | `AUTH_ADMIN_API_OAUTH2_ISSUER_URI` / `AUTH_ADMIN_API_OAUTH2_CLIENT_ID` | `auth.admin-api.oauth2.issuer-uri` / `.client-id` | issuer: `auth.adminApi.issuer`; client: `auth.adminApi.clientId` (`proxcloud-admin-api`) |

### Cloud providers

Proxmox VE clusters, Ceph RGW stores and the managed K8s/DBaaS management
clusters are **not** configured through env. They are external-service records
created in the admin console (System → Cloud providers) or as `externalService`
documents; see [cloud-integration.md](cloud-integration.md). One related key is
read from env:

| Concern | env | file | Default | Meaning |
|---------|-----|------|---------|---------|
| Default region | `PROXCLOUD_DEFAULT_REGION` | `proxcloud.default-region` | `default` | Region stamped on a provider binding when the provider sets none (`project/bootstrap.go`), and the admin handlers' region fallback. |

### Scheduled jobs (billing / metrics crons)

All **off by default** — a plain deploy stays dormant and never charges bills
unexpectedly.

| Concern | env | file | Default | Meaning |
|---------|-----|------|---------|---------|
| Scheduler | `PROXCLOUD_JOBS_SCHEDULER_ENABLED` | `proxcloud.jobs.scheduler-enabled` | `false` | Auto-start the charge/metrics crons (charges bills on a timer). |
| Debug triggers | `PROXCLOUD_JOBS_DEBUG_TRIGGERS` | `proxcloud.jobs.debug-triggers` | `false` | Expose on-demand `POST :8081/debug/run-*` triggers **without** starting the crons — for deterministic, manual runs. |
| Rabbit fanout | `PROXCLOUD_JOBS_RABBIT_FANOUT` | `proxcloud.jobs.rabbit-fanout` | `false` | Fan the **native** charge cron out across pods via RabbitMQ (one message per active profile) instead of charging every profile in one pod. Ignored by the external provider. |

Storage placement is configured per Proxmox provider too, not through env. The
keys live in the provider's `config` (full shape and behaviour:
[cloud-integration.md](cloud-integration.md#storage-capabilities-and-placement)):

| Key | Set by | Default | Meaning |
|-----|--------|---------|---------|
| `storageProfiles.<slug>` | Connection tab, Storage profiles block | — | `storages` (node or `*` → storage id), optional `label`, flags `shared` / `snapshots` / `thin` / `liveMigrate` / `replication`, caps `bootDisk` / `dbaas` (`false` closes the profile). Validated against a live detection on save. |
| `capabilities` | capability detection (sync cycle, Storage tab Re-detect, profile save) | absent until first detection | `{detectedAt, storages}`. Never hand-edited. |
| `storageClasses.<storage>` | Storage tab class select / `PUT /api/v1/admin/service/{id}/storage-classes`; installer | — | `class` = admin override (`performance` / `standard` / `capacity`, wins over the detected class); `suggestedClass` = installer fio result, shown only. |
| `backupStorage` | `PUT /api/v1/admin/service/{id}` (`{"config":{"backupStorage":"nas-backup"}}`); no UI field yet | none | The shared store server backups go to (NFS, CephFS or PBS, with `backup` content). Absent: the customer console offers no backups. The store is checked when a backup is created (409), not on save. |
| `importStorage` | `PUT /api/v1/admin/service/{id}` (`{"config":{"importStorage":"nas-templates"}}`); no UI field yet | none | The store with `import` content a node downloads customer images to. Absent: the customer console offers no image import. Test connection warns when the token cannot delete files there. |
| `storageHeadroomPct` | `PUT /api/v1/admin/service/{id}` (`{"config":{"storageHeadroomPct":15}}`); no UI field yet | `10` | Percent of a storage's total size that must stay free after placing a disk (server create, add volume, retype). `0` is allowed; a value that is not a number or outside 0–99 falls back to 10. |

Network MTU is set per EVPN zone on the Proxmox provider
(`config.network.zones.<zone>.mtu`, default 1450; vlan and simple zones take no MTU).
There is no deployment-wide MTU setting.

When the scheduler or debug triggers are enabled, the management port also exposes
operator triggers such as `POST :8081/debug/gen-hmac-key` (mint an Admin API key —
see `docs/auth.md`).

### Billing provider

ProxCloud ships a **complete billing system** — profiles, a rating engine, bills,
credits, savings plans, dunning and suspension — and it needs nothing but
PostgreSQL. That is the `native` provider and it is the default: check out this
repository, run it, and billing works.

Some deployments run a separate billing service instead. `external` delegates the
rating step to it. ProxCloud still resolves *what* to bill, because it owns the
resource cache and nothing else can.

| Concern | env | file | Default | Meaning |
|---------|-----|------|---------|---------|
| Provider | `PROXCLOUD_BILLING_PROVIDER` | `proxcloud.billing.provider` | `native` | `native` or `external`. An unrecognised value fails startup rather than silently defaulting. |
| Service URL | `PROXCLOUD_BILLING_URL` | `proxcloud.billing.url` | — | External only. In-cluster URL of the billing service. **Required** when provider is `external`. |
| Service token | `PROXCLOUD_BILLING_TOKEN` | `proxcloud.billing.token` | — | External only. Bearer token for that service's internal API. **Required** when provider is `external`. |
| Call timeout | `PROXCLOUD_BILLING_TIMEOUT` | — | `60s` | External only. Bounds one charge call, which carries every billable resource for a profile. |
| Callback token | `PROXCLOUD_BILLING_CALLBACK_TOKEN` | `proxcloud.billing.callback-token` | — | Shared secret the billing service presents when calling **back** into proxcloud to suspend/resume clouds, activate projects or send mail. Empty → those callbacks are not mounted at all. |

**What changes between the two.** Only the charge step. Every other billing
surface — the client REST API, the admin API, project cost reads, the billing
repository — behaves identically.

**What does not run under `external`.** The billing jobs that drive the in-tree
engine directly are not scheduled, because the billing service runs its own:
`savingsContractExpiration`, `savingsContractExpiryReminders`,
`reminderNotifications`, `paymentGatewayTransactionScanning`,
`autoSuspensionJob`, `monthlyBill`, `monthlyCollect`. Running both would mean two
dunning passes, two sets of customer emails and two attempts on the same saved
card. Non-billing jobs (metrics ingestion, cloud sync, project deletion) always
run — the external provider still needs this pod to keep the resource cache fresh.

> The external provider deliberately does **not** fall back to the native engine
> when the billing service is unreachable. Both engines write bills, and during a
> migration they may write to different databases, so a silent fallback could
> charge a cycle twice in two places. A failed run is easier to recover from.

### Mail / SMTP

Mail is configured via `PROXCLOUD_MAIL_*` **environment variables**
(`internal/platform/mail/mail.go`). If `PROXCLOUD_MAIL_SMTP_HOST` is unset the
service uses a no-op mailer and gated email side-effects silently do nothing.

| Concern | env | Default | Meaning |
|---------|-----|---------|---------|
| SMTP host | `PROXCLOUD_MAIL_SMTP_HOST` | — (unset ⇒ no-op mailer) | Relay host. |
| SMTP port | `PROXCLOUD_MAIL_SMTP_PORT` | `587` | Relay port. |
| SMTP username | `PROXCLOUD_MAIL_SMTP_USERNAME` | — | Relay auth user. |
| SMTP password | `PROXCLOUD_MAIL_SMTP_PASSWORD` | — | Relay auth password. |
| From address | `PROXCLOUD_MAIL_FROM` | — | Envelope-from / sender. |
| Business name | `PROXCLOUD_MAIL_BUSINESS_NAME` | `ProxCloud` | Sender display / branding in templates. |

Set these in Helm via the first-class `api.mail.smtp.*` block (plus `api.mail.from`),
which the chart injects into the pod as the `PROXCLOUD_MAIL_*` env vars above
(see `deploy/charts/proxcloud/templates/api/deployment.yaml`). Mail is env-only — nothing
renders into `application.yml`.

### Feature set

The available feature set is a fixed list in code (`internal/platform/feature`:
`billing`, `search`, `mailchimp`) and
is not config-driven. There is no license mechanism — ProxCloud is open-source and
imposes no licensed limits. The meaningful **runtime toggles** are the
scheduled-jobs flags above and per-region service enablement managed at runtime
(not via this config file).

## Bundled vs external dependencies

The chart can run everything in-cluster or point at your own managed services.
Toggle the bundled component off and fill in the matching `external*` block:

| Dependency | Bundle toggle | Bring-your-own |
|------------|---------------|----------------|
| PostgreSQL | `postgresql.enabled` (bitnami subchart) or `cnpg.enabled` (CloudNativePG operator) | `externalPostgresql.*` |
| RabbitMQ | `rabbitmq.enabled` | `externalRabbitmq.*` |
| Keycloak (OIDC) | `keycloakx.enabled` (codecentric/keycloakx) | set `auth.main.issuer` / `auth.admin.issuer` (+ `auth.adminApi.issuer`) to the external issuer |

For the deployment side — ingress, TLS, Gateway API routes, secrets wiring, and the
external-IdP checklist — see `deploy/charts/proxcloud/README.md` and
`deploy/charts/proxcloud/values.yaml` (a minimal starting point is in
`values-dev.yaml`).
