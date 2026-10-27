# Bills

Read-only access to billing-cycle bills. Base path: `/admin-api/v1/bills`.

## The bill object

| Field | Type | Description |
|---|---|---|
| `id` | string | Bill id. |
| `status` | string | Bill status (e.g. `OPEN`, `INVOICED`). |
| `currency` | string | Invoice currency. |
| `billing_profile_id` | string | The billed profile. |
| `items` | array | Always present. Line items (below). Empty on list responses unless `include_items=true`. |
| `start_date` | string (RFC 3339) | Billing cycle start. |
| `end_date` | string (RFC 3339) | Billing cycle end. |
| `created_at` | string (RFC 3339) | Creation timestamp. |
| `updated_at` | string (RFC 3339) | Last update timestamp. |

### The bill item object

| Field | Type | Description |
|---|---|---|
| `name` | string | Item display name. |
| `resource_id` | string | The billed resource. |
| `project_id` | string | The project the resource belongs to. |
| `resource_type` | string | Resource type (e.g. `INSTANCE`, `VOLUME`). |
| `attributes` | object | Resource metadata attached to the item (omitted when empty). |
| `currency` | string | Item currency. |
| `amount` | number | Net amount. |
| `created_at` | string (RFC 3339) | Item creation timestamp. |
| `updated_at` | string (RFC 3339) | Item update timestamp. |

## GET /admin-api/v1/bills

Lists bills, paginated. Line items are **excluded** by default (`items` comes back as `[]`) to keep responses small.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `billing_profile_id` | string | no | Filter by billed profile. |
| `status` | string | no | Exact status match. |
| `start_date` | string | no | RFC 3339 timestamp; only bills whose cycle starts at or after this instant. Invalid value → `400` `Invalid start_date`. |
| `end_date` | string | no | RFC 3339 timestamp; only bills whose cycle ends at or before this instant. Invalid value → `400` `Invalid end_date`. |
| `include_items` | boolean | no | `true` to include line items in list results (default `false`). |
| `limit` | integer | no | Page size (default 50, max 500). |
| `marker` | string | no | Keyset cursor. |

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  "https://<host>/admin-api/v1/bills?billing_profile_id=665f2a1bb8d34a0012340002&include_items=true"
```

Response `200`:

```json
{
  "data": [
    {
      "id": "665f4d2eb8d34a0012340020",
      "status": "INVOICED",
      "currency": "EUR",
      "billing_profile_id": "665f2a1bb8d34a0012340002",
      "items": [
        {
          "name": "instance-web-1",
          "resource_id": "665f4d2eb8d34a0012340021",
          "project_id": "665f3b0cb8d34a0012340010",
          "resource_type": "INSTANCE",
          "attributes": { "flavor": "m1.small" },
          "currency": "EUR",
          "amount": 12.40,
          "created_at": "2026-06-01T00:00:00Z",
          "updated_at": "2026-06-30T23:59:59Z"
        }
      ],
      "start_date": "2026-06-01T00:00:00Z",
      "end_date": "2026-07-01T00:00:00Z",
      "created_at": "2026-06-01T00:00:05Z",
      "updated_at": "2026-07-01T00:00:12Z"
    }
  ]
}
```

## GET /admin-api/v1/bills/{id}

Fetches one bill. Line items are always included.

Response `200`: `{ "data": { ...bill } }`.

Errors: `404 NOT_FOUND`.
