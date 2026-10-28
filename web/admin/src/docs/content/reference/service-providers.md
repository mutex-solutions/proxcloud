# Service Providers

Read-only access to the cloud service providers registered on the platform. Base path: `/admin-api/v1/service_providers`.

Credentials are never exposed. Only the provider's id, name, type and provider kind come back, plus the pool prefix for Proxmox VE providers.

## The service provider object

| Field | Type | Description |
|---|---|---|
| `id` | string | Provider id. |
| `name` | string | Display name. |
| `type` | string | `CLOUD` for cloud providers; omitted for other provider types. |
| `configuration` | object | Always present. |
| `configuration.cloud.provider` | string | The provider kind: `PROXMOX`, `CEPH_S3`, `KAMAJI` or `DBAAS`. |
| `configuration.cloud.proxmox` | object | Proxmox VE providers only. `pool_prefix` is the prefix of the per-project PVE pool names (`<pool_prefix><project id>`); it defaults to `pc-`. |

## GET /admin-api/v1/service_providers

Lists service providers, paginated.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `limit` | integer | no | Page size (default 50, max 500). |
| `marker` | string | no | Keyset cursor. |

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  "https://<host>/admin-api/v1/service_providers"
```

Response `200`:

```json
{
  "data": [
    {
      "id": "665f0000b8d34a0012340099",
      "name": "Proxmox Lab",
      "type": "CLOUD",
      "configuration": {
        "cloud": {
          "provider": "PROXMOX",
          "proxmox": { "pool_prefix": "pc-" }
        }
      }
    }
  ]
}
```

## GET /admin-api/v1/service_providers/{id}

Fetches one service provider. Response `200`: `{ "data": { ...provider } }`.

Errors: `404 NOT_FOUND` — `Service provider not found`.
