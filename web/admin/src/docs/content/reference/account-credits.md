# Account Credits

Manage prepaid / promotional credits attached to a billing profile. Base path: `/admin-api/v1/account_credits`.

## The account credit object

| Field | Type | Description |
|---|---|---|
| `id` | string | Credit id. |
| `billing_profile_id` | string | The profile the credit belongs to. |
| `initial_amount` | number | The amount the credit was created with. |
| `amount` | number | The remaining balance. |
| `currency` | string | The platform base currency the credit is denominated in. |
| `created_at` | string (RFC 3339) | Creation timestamp. |
| `updated_at` | string (RFC 3339) | Last update timestamp. |

## GET /admin-api/v1/account_credits

Lists a billing profile's credits, paginated.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `billing_profile_id` | string | yes | The profile whose credits to list. The profile is resolved first — an unknown (or missing) id returns `404`. |
| `limit` | integer | no | Page size (default 50, max 500). |
| `marker` | string | no | Keyset cursor. |

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  "https://<host>/admin-api/v1/account_credits?billing_profile_id=665f2a1bb8d34a0012340002"
```

Response `200`:

```json
{
  "data": [
    {
      "id": "665f5e3fb8d34a0012340030",
      "billing_profile_id": "665f2a1bb8d34a0012340002",
      "initial_amount": 100,
      "amount": 62.15,
      "currency": "EUR",
      "created_at": "2026-06-01T09:30:00Z",
      "updated_at": "2026-06-20T04:00:00Z"
    }
  ]
}
```

## GET /admin-api/v1/account_credits/{id}

Fetches one credit. Response `200`: `{ "data": { ...credit } }`.

Errors: `404 NOT_FOUND`.

## POST /admin-api/v1/account_credits

Grants a credit to a billing profile. The credit is denominated in the platform base currency; `initial_amount` and `amount` both start at the requested amount.

| Field | Type | Required | Description |
|---|---|---|---|
| `billing_profile_id` | string | yes | Must reference an existing billing profile. |
| `amount` | number | yes | The credit amount (decimal). |

Response `200`:

```json
{
  "data": {
    "id": "665f5e3fb8d34a0012340030",
    "billing_profile_id": "665f2a1bb8d34a0012340002",
    "initial_amount": 100,
    "amount": 100,
    "currency": "EUR",
    "created_at": "2026-07-03T10:00:00Z",
    "updated_at": "2026-07-03T10:00:00Z"
  }
}
```

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Malformed body, or `Invalid amount`. |
| `404 NOT_FOUND` | Billing profile doesn't exist. |
| `501 NOT_IMPLEMENTED` | **Not currently supported:** creating a credit for a profile whose invoice currency differs from the platform base currency (that would require an exchange-rate lookup). |

## DELETE /admin-api/v1/account_credits/{id}

Deletes a credit. Response `202` with an empty body.

Errors: `404 NOT_FOUND`.
