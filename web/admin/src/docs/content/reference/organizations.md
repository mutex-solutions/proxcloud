# Organizations

Manage organizations and their memberships. Base path: `/admin-api/v1/organizations`.

## The organization object

| Field | Type | Description |
|---|---|---|
| `id` | string | Organization id. |
| `name` | string | Display name. |
| `description` | string | Description (omitted when empty). |
| `billing_profile_id` | string | Attached billing profile (omitted when none). |
| `members` | array | Members, always present. Each entry is a member object (below). |
| `created_at` | string (RFC 3339) | Creation timestamp. |
| `updated_at` | string (RFC 3339) | Last update timestamp. |

### The member object

| Field | Type | Description |
|---|---|---|
| `sub` | string | The member's subject identifier. |
| `first_name` | string | Resolved from the user record (omitted when unknown). |
| `last_name` | string | Resolved from the user record (omitted when unknown). |
| `email` | string | Resolved from the user record (omitted when unknown). |
| `role` | string | Membership role, e.g. `OWNER`. |

## GET /admin-api/v1/organizations

Lists organizations, paginated.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | string | no | Exact name match. |
| `member_sub` | string | no | Only organizations the given `sub` belongs to. |
| `billing_profile_id` | string | no | Only organizations with this billing profile. |
| `limit` | integer | no | Page size (default 50, max 500). |
| `marker` | string | no | Keyset cursor. |

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  "https://<host>/admin-api/v1/organizations?limit=10"
```

Response `200`:

```json
{
  "data": [
    {
      "id": "665f2a1bb8d34a0012340001",
      "name": "Acme Corp",
      "description": "Primary tenant",
      "billing_profile_id": "665f2a1bb8d34a0012340002",
      "members": [
        {
          "sub": "user-9c4d1a2b3e4f5a6b7c8d9e0f1a2b3c4d",
          "first_name": "Jane",
          "last_name": "Doe",
          "email": "jane@example.com",
          "role": "OWNER"
        }
      ],
      "created_at": "2026-06-01T09:30:00Z",
      "updated_at": "2026-06-15T12:00:00Z"
    }
  ]
}
```

## GET /admin-api/v1/organizations/{id}

Fetches one organization. Response `200`: `{ "data": { ...organization } }`.

Errors: `404 NOT_FOUND`.

## POST /admin-api/v1/organizations

Creates an organization.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Display name. |
| `description` | string | no | Description. |
| `owner_sub` | string | no | If set, must resolve to an existing user; that user is added as `OWNER`. |
| `billing_profile_id` | string | no | Billing profile to attach. |

Response `200`: `{ "data": { ...organization } }`.

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Malformed body. |
| `404 NOT_FOUND` | `User not found` — `owner_sub` doesn't resolve to a user. |

## PUT /admin-api/v1/organizations/{id}

Updates an organization. **Partial update:** only fields present in the body overwrite; omitted fields keep their current value.

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | no | New name. |
| `description` | string | no | New description. |
| `billing_profile_id` | string | no | New billing profile id. |

Response `200`: `{ "data": { ...organization } }`.

Errors: `404 NOT_FOUND`, `400 BAD_REQUEST`.

## GET /admin-api/v1/organizations/{id}/members

Lists members. **Note:** this endpoint returns a bare JSON array, not the `data` envelope.

Response `200`:

```json
[
  {
    "sub": "user-9c4d1a2b3e4f5a6b7c8d9e0f1a2b3c4d",
    "first_name": "Jane",
    "last_name": "Doe",
    "email": "jane@example.com",
    "role": "OWNER"
  }
]
```

Errors: `404 NOT_FOUND` when the organization doesn't exist.

## POST /admin-api/v1/organizations/{id}/members

Adds a member.

| Field | Type | Required | Description |
|---|---|---|---|
| `sub` | string | yes | Must resolve to an existing user. |
| `role` | string | yes | Role to grant, e.g. `OWNER` or `MEMBER`. |

Response `201`: `{ "data": { ...member } }`.

Errors:

| Status | Condition |
|---|---|
| `404 NOT_FOUND` | Organization not found, or `User not found`. |
| `409 CONFLICT` | `User is already a member of this organization` |

## DELETE /admin-api/v1/organizations/{id}/members/{sub}

Removes a member. Response `204` with an empty body.

Errors:

| Status | Condition |
|---|---|
| `404 NOT_FOUND` | Organization not found, or `Member not found`. |
| `409 CONFLICT` | `Cannot remove the last owner` |

## PUT /admin-api/v1/organizations/{id}/members/{sub}/role

Changes a member's role.

| Field | Type | Required | Description |
|---|---|---|---|
| `role` | string | yes | The new role. |

Response `200`: `{ "data": { ...member } }` with the updated `role`.

Errors:

| Status | Condition |
|---|---|
| `404 NOT_FOUND` | Organization not found, or `Member not found`. |
| `409 CONFLICT` | `Cannot change role of the last owner` — demoting the only `OWNER`. |
