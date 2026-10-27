# Service Providers

Read-only access to the cloud service providers registered on the platform. Base path: `/admin-api/v1/service_providers`.

Credentials are never exposed — only the provider's identity endpoint and customer domain id come back.

## The service provider object

| Field | Type | Description |
|---|---|---|
| `id` | string | Provider id. |
| `name` | string | Display name. |
| `type` | string | `CLOUD` for cloud providers; omitted for other provider types. |
| `configuration` | object | Always present. |
| `configuration.cloud.provider` | string | Always `OPENSTACK`. |
| `configuration.cloud.openstack.identity_url` | string | The Keystone identity endpoint, as configured. |
| `configuration.cloud.openstack.domain_id` | string | The customer domain id (omitted when not configured). |

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
      "name": "Platform Cloud",
      "type": "CLOUD",
      "configuration": {
        "cloud": {
          "provider": "OPENSTACK",
          "openstack": {
            "identity_url": "https://keystone.example.com:5000",
            "domain_id": "default"
          }
        }
      }
    }
  ]
}
```

## GET /admin-api/v1/service_providers/{id}

Fetches one service provider. Response `200`: `{ "data": { ...provider } }`.

Errors: `404 NOT_FOUND` — `Service provider not found`.
