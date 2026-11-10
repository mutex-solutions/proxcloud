# Backup and Recovery

ProxCloud holds your billing truth — customers, balances, invoices, resource usage — so losing it is not an option. Get backups in place before you onboard real customers. This page spells out exactly what state exists, which parts are irreplaceable, and two workable ways to protect them.

## What has to be protected

| State | Where it lives | Criticality |
|-------|----------------|-------------|
| Platform data (customers, projects, resources, bills, transactions), durable jobs/receipts and configured outbox decisions | PostgreSQL (the writable primary `proxcloud` database) | Primary state — back up continuously; preserve finance and work state consistently |
| Identity (user accounts, credentials, realm config) | Keycloak's own database | Required for users to keep signing in |
| **Encryption key** | Kubernetes secret `<release>-api`, key `encryption-key` | **CRITICAL — unrecoverable if lost** |
| Helm configuration | Version-controlled **secret-free** values plus separately vaulted secret values/Secret references | Needed to rebuild; never commit encryption keys, passwords or populated secret overlays |

Current ProxCloud does not require RabbitMQ: native billing work and its financial receipts live on PostgreSQL primary. **A legacy broker is not disposable transient state.** Before a broker-free upgrade, stop producers/consumers, drain or reconcile ready and unacknowledged work, and preserve the broker volumes and evidence until the migration is accepted. Do not use `down -v`, delete its PVCs, or assume lost messages can be reconstructed.

Application pods can be recreated by Helm, but their configuration, credentials, PostgreSQL state and any retained legacy broker state must be protected. Do not restore a live writable-primary database by merely copying its mounted files; use PostgreSQL-consistent backups/dumps or the database operator's supported backup procedure.

## The encryption key

The API encrypts sensitive fields at rest with `PROXCLOUD_ENCRYPTION_DEFAULT_KEY`, held in the Kubernetes secret `<release>-api` under the key `encryption-key` (sourced from `api.encryptionKey`, or your own `api.existingSecret`). **A PostgreSQL backup without this key is only half a backup — the encrypted fields inside it can never be decrypted again.**

Export it now and keep it in your password manager or vault, outside the cluster:

```sh
kubectl -n proxcloud get secret proxcloud-api \
  -o jsonpath='{.data.encryption-key}' | base64 -d
```

On a rebuilt cluster, feed it back through values before installing — either inline or via a pre-created secret:

```yaml
api:
  encryptionKey: "<the exported value>"
  # or supply your own secret carrying db-url and encryption-key:
  # optional db-read-url is history-only; add sso-client-secret when SSO is enabled.
  # existingSecret: my-restored-api-secret
```

## Approach 1 — Kubernetes recovery with Velero, plus a database backup

Velero can preserve namespace objects — Deployments, StatefulSets, Secrets (including `<release>-api`), ConfigMaps and Ingresses — and filesystem volume data via its node agent. **The example below does not quiesce PostgreSQL or Keycloak databases and is not a standalone consistent database backup.** Pair it with operator-supported continuous/WAL backups or a consistent dump and a tested restore plan; do not treat a live primary filesystem copy as recoverable financial truth. Keep the backup bucket outside the cluster's failure domain and protect its secret-bearing contents.

An example schedule — hourly, 10-day retention:

```sh
velero install \
  --provider aws --plugins velero/velero-plugin-for-aws \
  --bucket proxcloud-backups --secret-file ./s3-credentials \
  --backup-location-config region=eu-1,s3ForcePathStyle=true,s3Url=https://s3.example.com \
  --use-node-agent --default-volumes-to-fs-backup

velero schedule create proxcloud-hourly \
  --schedule "0 * * * *" --ttl 240h \
  --include-namespaces proxcloud
```

The following restores the backed-up Kubernetes objects/volume copies. Before running it, prevent application replicas from starting against an unverified restored database: restoring Deployments can immediately start durable workers. Select/exclude resources or use an isolated recovery cluster and the database operator's restore procedure as your tested plan requires:

```sh
velero restore create --from-backup proxcloud-hourly-<timestamp>
```

Restoring Kubernetes objects is not proof of a usable or financially consistent recovery. Restore the matching database backup and encryption key first, keep new producers off, and verify database health, existing durable work, identity and ingress before admitting customers. Bounded durable workers start even when the scheduler is off; restoring queued work can resume processing.

## Approach 2 — targeted database dumps

If you prefer classic database backups — or your PostgreSQL is external and already has its own backup regime:

```sh
# Platform data — the primary state. Default CloudNativePG cluster <release>-pg:
# run it on the current primary (status.currentPrimary of the Cluster).
PRIMARY=$(kubectl -n proxcloud get cluster proxcloud-pg -o jsonpath='{.status.currentPrimary}')
kubectl -n proxcloud exec "$PRIMARY" -c postgres -- \
  pg_dump -d proxcloud --format=custom \
  > proxcloud-$(date +%F-%H%M).dump
# (bitnami subchart instead: kubectl -n proxcloud exec proxcloud-postgresql-0 -- \
#   env PGPASSWORD=$PG_PASS pg_dump -U proxcloud -d proxcloud --format=custom)

# Keycloak database — identity (adjust to your Keycloak's DB pod/name)
kubectl -n proxcloud exec <keycloak-db-pod> -- \
  env PGPASSWORD=$KC_PASS pg_dump -U keycloak keycloak \
  | gzip > keycloak-$(date +%F-%H%M).sql.gz
```

Run these from a CronJob or an external runner and ship the archives off-cluster. Restore is `pg_restore` (for the custom-format dump) / `psql` into fresh databases, then `helm upgrade --install` with your saved `values.yaml` and the restored `encryption-key`.

## Rehearse the restore

A backup you've never restored is only a hypothesis. Periodically rehearse into a scratch namespace or cluster: restore PostgreSQL + Keycloak + the encryption key, install the chart, and confirm you can log in and open a customer's billing history.
