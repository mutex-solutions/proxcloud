# Backup and Recovery

ProxCloud holds your billing truth — customers, balances, invoices, resource usage — so losing it is not an option. Get backups in place before you onboard real customers. This page spells out exactly what state exists, which parts are irreplaceable, and two workable ways to protect them.

## What has to be protected

| State | Where it lives | Criticality |
|-------|----------------|-------------|
| Platform data (customers, projects, resources, bills, transactions) | PostgreSQL (the `proxcloud` database) | Primary state — back up continuously |
| Identity (user accounts, credentials, realm config) | Keycloak's own database | Required for users to keep signing in |
| **Encryption key** | Kubernetes secret `<release>-api`, key `encryption-key` | **CRITICAL — unrecoverable if lost** |
| Helm values | your `values.yaml` (keep it in git) | Needed to rebuild the deployment |

Not worth backing up: RabbitMQ contents (transient messaging that repopulates) and the application pods themselves (Helm recreates them).

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
  # or supply your own secret carrying db-url, rabbitmq-password, encryption-key:
  # existingSecret: my-restored-api-secret
```

## Approach 1 — Velero (whole-namespace)

Velero snapshots the entire `proxcloud` namespace — Deployments, StatefulSets, Secrets (including `<release>-api`), ConfigMaps, Ingresses — and copies persistent-volume contents via its file-system backup (node agent) to an S3-compatible bucket. Keep that bucket **outside** the cluster's failure domain for genuine disaster recovery.

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

Restore brings back the volumes and every Kubernetes object in a single move:

```sh
velero restore create --from-backup proxcloud-hourly-<timestamp>
```

Pods, ConfigMaps, Secrets and Ingress come up together with the data, so the platform is usable as soon as the restore completes.

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
