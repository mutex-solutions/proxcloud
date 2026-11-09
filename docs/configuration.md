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
to start without `PROXCLOUD_DB_URL` pointing at a **writable primary**. RabbitMQ
is no longer required. A true legacy fanout flag is a startup **migration error**;
follow [the cutover runbook](postgres-jobs-migration.md) before clearing it.

## Configuration reference

Legend: **env** = environment variable, **file** = `application.yml` key, **helm**
= `values.yaml` path.

### Server

| Concern | env | file | helm | Default | Meaning |
|---------|-----|------|------|---------|---------|
| API port | — | `server.port` | fixed `8080` | `8080` | Public API listener. |
| Management port | — | `management.server.port` | fixed `8081` | `8081` | Health probes + operator debug triggers. |
| Log level | — | `logging.level.root` | through `api.applicationYml` | `INFO` | Root log level. |
| CORS origins | `PROXCLOUD_CORS_ALLOWED_ORIGINS` | — | `api.corsAllowedOrigins` | the origins of the UI and admin self URLs | Extra browser origins allowed to call the API (comma list), on top of the two consoles. |

### PostgreSQL (required)

| Concern | env | file | helm | Default | Meaning |
|---------|-----|------|------|---------|---------|
| Primary DSN | `PROXCLOUD_DB_URL` | `db.url` | derived from `cnpg.*`, `postgresql.*` or `externalPostgresql.*`; Secret `db-url` | — (required) | **Writable primary**, also used for durable jobs, leases, retries and all finance. CNPG always uses `-rw`. Supply credentials through a Secret, not a committed DSN example. |
| Optional reader | `PROXCLOUD_DB_READ_URL` | `db.read-url` | optional `db-read-url` in `api.existingSecret`, `externalPostgresql.readExistingSecret` / `readSecretKey`, or explicit `cnpg.statusReadReplica` | empty → primary | **History/metrics only**; never worker/finance input or writes. A configured failed reader makes those reads fail 503, not silently fall back. |

PostgreSQL is the sole primary datastore: every document kind is a
`(id text primary key, doc jsonb not null)` table (see
[data-model.md](data-model.md)). The chart builds the DSN from the CloudNativePG
cluster (`cnpg.enabled`, the default), the bitnami `postgresql` subchart, or
`externalPostgresql.*` when both are off, and injects it as env;
the credentials come from a secret (or `externalPostgresql.existingSecret`).
Documents are (de)serialized with the standard-library `encoding/json` (no ORM
or external codec) and stored verbatim in each table's `doc` jsonb column.

The optional reader is not a general read/write split. Leave it empty when no
replica exists. CNPG `statusReadReplica: false` is the default; opt-in with
`cnpg.instances >= 2` derives `-ro` only for that status reader. An existing API
Secret may omit `db-read-url` (optional key reference, primary fallback). An
explicit external reader Secret/key must exist. Seed jobs use the primary only.

### RabbitMQ (deprecated; migration retention only)

The API does not connect to RabbitMQ. Old `PROXCLOUD_RABBITMQ_{HOST,PORT,USERNAME,PASSWORD}`
/ `rabbitmq.*` file settings are ignored with a warning. Chart broker toggles
now default false and require explicit legacy-retain migration values; they do
not wire broker credentials into the API. Existing queues/PVCs are not deleted.
Protect the live broker **before** removing it from desired state, with a retained
Helm release phase and Argo pruning protection where applicable. See
[the drain/reconcile runbook](postgres-jobs-migration.md); flag false alone is
not evidence that the old queue is empty.

### Encryption (data-at-rest key)

| Concern | env | file | helm | Default | Meaning |
|---------|-----|------|------|---------|---------|
| Default key | `PROXCLOUD_ENCRYPTION_DEFAULT_KEY` | `proxcloud.encryption.default.key` (alias `default-key`) | `api.encryptionKey` (or `api.existingSecret`) | — | Key used to encrypt sensitive fields at rest. |

The chart renders `api.encryptionKey` into a Secret and injects it as
`PROXCLOUD_ENCRYPTION_DEFAULT_KEY`. It protects provider and integration secrets and the secrets of admin hmac keys, and
changing it locks all of those out: hmac key rows that no longer decrypt fail authentication until they are minted again.
Alternatively, point `api.existingSecret` at a
pre-made Secret carrying `db-url` / `encryption-key` (and `sso-client-secret` when
SSO is enabled). No broker credential or replica key is required.

### Self URLs

Used to build absolute links (mail, redirects, MCP resource metadata).

| Concern | env | file | helm | Meaning |
|---------|-----|------|------|---------|
| Base URL | `PROXCLOUD_SELF_BASE_URL` | `proxcloud.self.base-url` | `api.selfUrls.base` | Overall public base URL. |
| API base URL | `PROXCLOUD_SELF_API_BASE_URL` | `proxcloud.self.api-base-url` | `api.selfUrls.api` | Public API URL (also the MCP resource base). |
| UI base URL | `PROXCLOUD_SELF_UI_BASE_URL` | `proxcloud.self.ui-base-url` | `api.selfUrls.ui` | Customer console URL. |
| Admin base URL | `PROXCLOUD_SELF_ADMIN_BASE_URL` | `proxcloud.self.admin-base-url` | `api.selfUrls.admin` | Admin console URL. |

### OIDC realms

Three realm configs, each an issuer URI + client id, and an optional discovery URI
(where the API fetches the OIDC metadata when the public issuer is not reachable from
the pod; the token `iss` must still equal the issuer). See `docs/auth.md` for the
identity model. Empty issuer = realm disabled (tokens for it are rejected); with an
empty issuer the chart renders no env for that realm at all.

| Realm | env (issuer / client / discovery) | file | helm |
|-------|-----------------------|------|------|
| Customer (`auth.main`) | `AUTH_MAIN_OAUTH2_ISSUER_URI` / `_CLIENT_ID` / `_DISCOVERY_URI` | `auth.main.oauth2.issuer-uri` / `.client-id` / `.discovery-uri` | `auth.main.issuer`, `.clientId` (`proxcloud-ui`), `.discoveryUri` |
| Operator (`auth.admin`) | `AUTH_ADMIN_OAUTH2_ISSUER_URI` / `_CLIENT_ID` / `_DISCOVERY_URI` | `auth.admin.oauth2.issuer-uri` / `.client-id` / `.discovery-uri` | `auth.admin.issuer`, `.clientId` (`proxcloud-admin`), `.discoveryUri` |
| Admin API (`auth.admin-api`) | `AUTH_ADMIN_API_OAUTH2_ISSUER_URI` / `_CLIENT_ID` / `_DISCOVERY_URI` | `auth.admin-api.oauth2.issuer-uri` / `.client-id` / `.discovery-uri` | `auth.adminApi.issuer`, `.clientId` (`proxcloud-admin-api`), `.discoveryUri` |

The installer points `auth.main` at the Keycloak realm `proxcloud` and `auth.admin` and
`auth.admin-api` at `proxcloud-admin` (`keycloak.main_realm` / `admin_realm`); the dev
compose stack uses one realm, `proxcloud`, for all three.

### Per-organization SSO

| Concern | env | helm | Default | Meaning |
|---------|-----|------|---------|---------|
| Keycloak client | `PROXCLOUD_SSO_KC_CLIENT_ID` | — | `proxcloud-idp-admin` | Service-account client the API uses to manage organizations' identity providers. |
| Client secret | `PROXCLOUD_SSO_KC_CLIENT_SECRET` | `auth.sso.clientSecret` (or `sso-client-secret` in `api.existingSecret`) | — | Its secret. The chart sets the SSO env only with `auth.sso.enabled`. |
| Reserved domains | `PROXCLOUD_SSO_RESERVED_DOMAINS` | `auth.sso.reservedDomains` | — | Email domains no organization may claim (comma list); the self URL domains are always reserved. |
| Internal IdPs | `PROXCLOUD_SSO_ALLOW_INTERNAL_IDP` | `auth.sso.allowInternalIdp` | `false` | Development only: allows a customer IdP on http or a private address. |

### Cloud providers

Proxmox VE clusters, Ceph RGW stores and the managed K8s/DBaaS management
clusters are **not** configured through env. They are external-service records
created in the admin console (System → Cloud providers) or as `externalService`
documents; see [cloud-integration.md](cloud-integration.md). One related key is
read from env:

| Concern | env | file | Default | Meaning |
|---------|-----|------|---------|---------|
| Default region | `PROXCLOUD_DEFAULT_REGION` | `proxcloud.default-region` | `default` | Region stamped on a provider binding when the provider sets none (`project/bootstrap.go`), and the admin handlers' region fallback. |
| CSI gateway address | `PROXCLOUD_CSI_GATEWAY_ADDR` | `proxcloud.csi-gateway.addr` | — (off) | Listener for tenant clusters' proxmox-csi controllers (e.g. `:8443`), see [managed-k8s.md](managed-k8s.md). Separate from `:8080` and `:8081` on purpose. |
| CSI gateway certificate | `PROXCLOUD_CSI_GATEWAY_TLS_CERT`, `PROXCLOUD_CSI_GATEWAY_TLS_KEY` | `proxcloud.csi-gateway.tls-cert`, `.tls-key` | — | PEM files. The listener serves TLS only; an address without both fails startup. Read again a minute after the last handshake, so a renewal in place needs no restart. |
| CSI gateway URL and CA | `PROXCLOUD_CSI_GATEWAY_URL`, `PROXCLOUD_CSI_GATEWAY_CA_FILE` | `proxcloud.csi-gateway.url`, `.ca-file` | — (off) | `https://<host>:<port>` as the services cluster reaches the gateway, and the PEM CA that signs its certificate. Set, new Kubernetes clusters run their proxmox-csi controller on the services cluster against the gateway; empty, they get no proxmox-csi (except on `sharedWorkers`). Required together. Independent of the address: the replica that creates a cluster need not serve the gateway. |

In Helm the gateway rows come from `api.csiGateway.*`: `enabled` sets the address from
`port` (default `8443`), `url` the URL, and the certificate, key and CA are a cert-manager
certificate the chart mounts; `service.*` and `allowedCIDRs` expose the port to the
services cluster's nodes only.

The gateway's PVE token is per compute provider: `config.csiGatewayTokenId` and `secret.csiGatewaySecret` on the Proxmox
provider ([proxmox-permissions.md](proxmox-permissions.md#the-csi-gateway-token)). Without them the gateway answers its
clusters with an error.

### Scheduled jobs (billing / metrics crons)

The **scheduler and debug triggers are off by default**: a fresh deploy creates
no timed billing work. Durable workers nevertheless start on every API pod and
finish **already-enqueued** work. These are producer/route gates, not a queue
pause, and there is no `QUEUE_ENABLED` setting.

| Concern | env | file | Default | Meaning |
|---------|-----|------|---------|---------|
| Scheduler | `PROXCLOUD_JOBS_SCHEDULER_ENABLED` | `proxcloud.jobs.scheduler-enabled` | `false` | Auto-start the charge/metrics crons (charges bills on a timer). |
| Debug triggers | `PROXCLOUD_JOBS_DEBUG_TRIGGERS` | `proxcloud.jobs.debug-triggers` | `false` | Expose on-demand `POST :8081/debug/run-*` triggers **without** starting the crons — for deterministic, manual runs. |
| Worker concurrency | `PROXCLOUD_JOBS_WORKER_CONCURRENCY` | — (env only) | `2` | `1..16` bounded workers per API pod (`api.jobs.workerConcurrency`). Always active, using the writable primary. |
| Poll interval | `PROXCLOUD_JOBS_POLL_INTERVAL` | — (env only) | `1s` | Go duration, `100ms..1m` (`api.jobs.pollInterval`). |
| Lease duration | `PROXCLOUD_JOBS_LEASE_DURATION` | — (env only) | `1m` | Go duration, `10s..10m` (`api.jobs.leaseDuration`); engine uses database-time owner+epoch CAS fencing. |
| Legacy Rabbit fanout | `PROXCLOUD_JOBS_RABBIT_FANOUT` | `proxcloud.jobs.rabbit-fanout` | `false` | **true refuses startup**, including a true YAML setting. Stop producers, drain/reconcile and record old queue evidence before clearing. |

Storage placement is configured per Proxmox provider too, not through env. The
keys live in the provider's `config` (full shape and behaviour:
[cloud-integration.md](cloud-integration.md#storage-capabilities-and-placement)):

| Key | Set by | Default | Meaning |
|-----|--------|---------|---------|
| `storageProfiles.<slug>` | Connection tab, Storage profiles block | — | `storages` (node or `*` → storage id), optional `label`, flags `shared` / `snapshots` / `thin` / `liveMigrate` / `replication`, caps `bootDisk` / `dbaas` (`false` closes the profile). Validated against a live detection on save. |
| `capabilities` | capability detection (sync cycle, Storage tab Re-detect, profile save) | absent until first detection | `{detectedAt, storages}`. Never hand-edited. |
| `storageClasses.<storage>` | Storage tab class select / `PUT /api/v1/admin/service/{id}/storage-classes`; installer | — | `class` = admin override (`performance` / `standard` / `capacity`, wins over the detected class); `suggestedClass` = installer fio result, shown only. |
| `backupStorage` | installer (stage 60, with a NAS `backup` export); `PUT /api/v1/admin/service/{id}` (`{"config":{"backupStorage":"nas-backup"}}`); no UI field yet | none | The shared store server backups go to (NFS, CephFS or PBS, with `backup` content). Absent: the customer console offers no backups. The store is checked when a backup is created (409), not on save. |
| `importStorage` | installer (stage 60, with a NAS `templates` export); `PUT /api/v1/admin/service/{id}` (`{"config":{"importStorage":"nas-templates"}}`); no UI field yet | none | The store with `import` content the portal streams customer images to. Absent: the customer console offers no image import. Test connection warns when the token cannot delete files there. |
| `importUnknownSizeMaxGiB` | `PUT /api/v1/admin/service/{id}` (`{"config":{"importUnknownSizeMaxGiB":20}}`); no UI field yet | none | A URL whose server states no size is refused unless this is set. Such an image is spooled to the portal's temporary directory up to this limit, so size that disk for it. |
| `storageHeadroomPct` | `PUT /api/v1/admin/service/{id}` (`{"config":{"storageHeadroomPct":15}}`); no UI field yet | `10` | Percent of a storage's total size that must stay free after placing a disk (server create, add volume, retype). `0` is allowed; a value that is not a number or outside 0–99 falls back to 10. |
| `isoStorage` | `PUT /api/v1/admin/service/{id}`; no UI field yet | `local` | Node-local store where a Kubernetes node's bootstrap ISO (its kubeadm join token) lands. Never a shared store; keep it in line with the installer's `services.iso_storage`, see [managed-k8s.md](managed-k8s.md). |
| `lbImage` | `PUT /api/v1/admin/service/{id}`; no UI field yet | `debian-13` | Template a load balancer VM is cloned from (Debian family, guest agent and cloud-init). |
| `csiGatewayTokenId` | installer (stage 60, split mode: `csi@pve!gateway`); with `secret.csiGatewaySecret` | none | The CSI gateway's PVE token for this provider, see below. |

Network MTU is set per EVPN zone on the Proxmox provider
(`config.network.zones.<zone>.mtu`, default 1450; vlan and simple zones take no MTU).
There is no deployment-wide MTU setting.

With debug triggers enabled (`PROXCLOUD_JOBS_DEBUG_TRIGGERS`, not the scheduler), the
management port also exposes `POST :8081/debug/gen-hmac-key` (mint an Admin API key,
see `docs/auth.md`). It is unauthenticated, which is why the scheduler alone never
turns it on.

### Durable savings reminder enqueue (not delivery)

`PROXCLOUD_MAIL_REMINDER_OUTBOX_ENABLED` defaults **false** (environment only).
True requires a >=32-byte `PROXCLOUD_ENCRYPTION_DEFAULT_KEY` and opts savings-expiry
reminders into atomic rendered/encrypted outbox decisions on PRIMARY. It starts
neither a scheduler nor an SMTP dispatcher; configured gateways do not enable
sending. Queue-only rollout cannot yet deliver these reminders. Other mail remains
best-effort. Existing durable decisions never fall back to direct SMTP when the
flag is switched OFF, and keyless cancellation remains active. No production
recipient rollout or backfill is implicit. See [safety and rollback limits](mail-outbox-safety-limits.md).

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

The mail gateway is resolved on every send. An SMTP integration set in the admin
console (Admin → Integrations, stored in the database) wins; otherwise the
`PROXCLOUD_MAIL_*` **environment variables** below (`internal/platform/mail/mail.go`)
are the fallback. With neither (no integration and `PROXCLOUD_MAIL_SMTP_HOST`
unset) the service uses a no-op mailer and email side-effects silently do nothing.

| Concern | env | Default | Meaning |
|---------|-----|---------|---------|
| SMTP host | `PROXCLOUD_MAIL_SMTP_HOST` | — (unset ⇒ no-op mailer) | Relay host. |
| SMTP port | `PROXCLOUD_MAIL_SMTP_PORT` | `587` | Relay port. |
| SMTP username | `PROXCLOUD_MAIL_SMTP_USERNAME` | — | Relay auth user. |
| SMTP password | `PROXCLOUD_MAIL_SMTP_PASSWORD` | — | Relay auth password. |
| From address | `PROXCLOUD_MAIL_FROM` | — | Envelope-from / sender. |
| Business name | `PROXCLOUD_MAIL_BUSINESS_NAME` | `ProxCloud` | Sender display / branding in templates. |

Set these in Helm via the first-class `api.mail.smtp.*` block (plus `api.mail.from` and `api.mail.businessName`),
which the chart injects into the pod as the `PROXCLOUD_MAIL_*` env vars above
(see `deploy/charts/proxcloud/templates/api/deployment.yaml`). Nothing about mail
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
| PostgreSQL | `cnpg.enabled` (CloudNativePG cluster, the default; needs the operator) or `postgresql.enabled` (bitnami subchart) | `externalPostgresql.*` (both toggles off) |
| Legacy RabbitMQ | both `rabbitmqCluster.enabled` and deprecated `rabbitmq.enabled` default **false**; explicit migration retain only | no API broker connection; old `externalRabbitmq.*` accepted but unused |
| Keycloak (OIDC) | `keycloakx.enabled` (codecentric/keycloakx) | set `auth.main.issuer` / `auth.admin.issuer` (+ `auth.adminApi.issuer`) to the external issuer |

For the deployment side — ingress, TLS, Gateway API routes, secrets wiring, and the
external-IdP checklist — see `deploy/charts/proxcloud/README.md` and
`deploy/charts/proxcloud/values.yaml` (a minimal starting point is in
`values-dev.yaml`).
