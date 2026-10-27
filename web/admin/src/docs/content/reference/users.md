# Users

Manage platform user accounts. Base path: `/admin-api/v1/users`.

## The user object

| Field | Type | Description |
|---|---|---|
| `id` | string | User id. |
| `sub` | string | Primary subject identifier. |
| `first_name` | string | First name (omitted when empty). |
| `last_name` | string | Last name (omitted when empty). |
| `email` | string | Email address. |
| `identities` | array | Linked identities, always present. Each entry has `sub` and `issuer`. |

## GET /admin-api/v1/users

Lists users, paginated.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `email` | string | no | Exact email match. |
| `sub` | string | no | Matches the primary `sub` or any linked identity's `sub`. |
| `limit` | integer | no | Page size (default 50, max 500). |
| `marker` | string | no | Keyset cursor from the previous page's `next_marker`. |

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  "https://<host>/admin-api/v1/users?email=jane@example.com"
```

Response `200`:

```json
{
  "data": [
    {
      "id": "665f1c2ab8d34a0012345678",
      "sub": "user-9c4d1a2b3e4f5a6b7c8d9e0f1a2b3c4d",
      "first_name": "Jane",
      "last_name": "Doe",
      "email": "jane@example.com",
      "identities": [
        { "sub": "user-9c4d1a2b3e4f5a6b7c8d9e0f1a2b3c4d", "issuer": "api" }
      ]
    }
  ]
}
```

## GET /admin-api/v1/users/{id}

Fetches one user.

Response `200`: `{ "data": { ...user } }` — same shape as above.

Errors: `404 NOT_FOUND` when the id doesn't exist.

## POST /admin-api/v1/users

Pre-creates a user ahead of their first login.

| Field | Type | Required | Description |
|---|---|---|---|
| `email` | string | yes | The user's email. Must not already exist. |
| `sub` | string | no | Subject identifier. Defaults to a generated `user-<32 hex>` value. |

Response `200`:

```json
{
  "data": {
    "id": "665f1c2ab8d34a0012345678",
    "sub": "user-9c4d1a2b3e4f5a6b7c8d9e0f1a2b3c4d",
    "email": "jane@example.com",
    "identities": [
      { "sub": "user-9c4d1a2b3e4f5a6b7c8d9e0f1a2b3c4d", "issuer": "api" }
    ]
  }
}
```

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Malformed body. |
| `409 CONFLICT` | `A user with this email already exists.` |

## DELETE /admin-api/v1/users/{id}

Deletes a user.

Response `200` with an empty body.

Errors: `404 NOT_FOUND` when the id doesn't exist.
