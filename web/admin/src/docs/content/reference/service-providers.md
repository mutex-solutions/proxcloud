# Service Providers

Read-only access to the cloud service providers registered on the platform. Base path: `/admin-api/v1/service_providers`.

Credentials are never exposed. Only the provider's id, name and type come back, inside a configuration object whose shape is kept from an earlier backend for compatibility.

## The service provider object

| Field | Type | Description |
|---|---|---|
| `id` | string | Provider id. |
| `name` | string | Display name. |
| `type` | string | `CLOUD` for cloud providers; omitted for other provider types. |
| `configuration` | object | Always present. |
| `configuration.cloud.provider` | string | Always `OPENSTACK`. This is a fixed legacy value and is returned for Proxmox VE, Ceph S3, Kamaji and DBaaS providers alike. |
| `configuration.cloud.openstack` | object | Always present. Its legacy fields `identity_url` and `domain_id` are omitted for providers that don't set them, which includes every Proxmox VE provider, so it comes back as `{}`. |

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
          "provider": "OPENSTACK",
          "openstack": {}
        }
      }
    }
  ]
}
```

## GET /admin-api/v1/service_providers/{id}

Fetches one service provider. Response `200`: `{ "data": { ...provider } }`.

Errors: `404 NOT_FOUND` — `Service provider not found`.
