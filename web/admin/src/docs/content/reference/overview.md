# Admin API Reference

The ProxCloud Admin API is a machine-to-machine REST interface for managing users, organizations, projects, billing profiles, bills, account credits, and service providers.

- **Base URL:** `https://<your-proxcloud-host>/admin-api/v1`
- **Format:** JSON with `snake_case` field names
- **Auth:** AWS Signature Version 4 over an HMAC key pair — see [Authentication](/docs/reference/authentication)

## Authentication overview

Every request has to be signed with AWS SigV4 using an HMAC key pair — an access key id (`pk…`) and a secret key (`sk…`) — that you generate on the admin UI's **System → API keys** page. As an alternative, a bearer token from the dedicated admin-api OIDC realm is accepted. Requests that are unsigned or signed incorrectly get `401`; a bearer token from any other realm gets `403`.

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  "https://<host>/admin-api/v1/users?limit=10"
```

## Response envelopes

A single entity is wrapped in `data`:

```json
{ "data": { "id": "665f1c2ab8d34a0012345678", "email": "jane@example.com" } }
```

A list is also wrapped in `data`, with `next_marker` present only when there's another page to fetch:

```json
{ "data": [ { "id": "..." } ], "next_marker": "665f1c2ab8d34a0012349999" }
```

Errors carry an `error` object:

```json
{ "error": { "code": "NOT_FOUND", "message": "Not Found" } }
```

Empty-valued fields are dropped from responses (`NON_NULL` serialization). Array fields — `members`, `identities`, `items`, `provisioned_services` — are always present even when empty.

## Pagination (keyset marker)

List endpoints paginate by keyset:

| Parameter | Type | Default | Description |
|---|---|---|---|
| `limit` | integer | `50` | Page size, 1–500. Values ≤ 0 or missing fall back to 50. A non-numeric value returns `400`; a value over 500 returns `400` with `page limit can't exceed 500`. |
| `marker` | string | — | An opaque cursor: the id of the first item of the next page, taken from the previous response's `next_marker`. |

Keep passing each response's `next_marker` back as `marker` until `next_marker` is no longer returned.

## Error model

| Status | Code | Meaning |
|---|---|---|
| `400` | `BAD_REQUEST` | Malformed JSON body, an invalid parameter, or any processing failure. |
| `401` | — | Missing or invalid SigV4 signature / bearer token (empty body). |
| `403` | — | Authenticated, but the credential isn't allowed on the Admin API (empty body). |
| `404` | `NOT_FOUND` | The resource doesn't exist. |
| `409` | `CONFLICT` | A state conflict (duplicate email, last owner, existing member). |
| `501` | `NOT_IMPLEMENTED` | The operation isn't supported on this deployment. |

## The resources

| Resource | Base path | Reference |
|---|---|---|
| Users | `/admin-api/v1/users` | [Users](/docs/reference/users) |
| Organizations | `/admin-api/v1/organizations` | [Organizations](/docs/reference/organizations) |
| Projects | `/admin-api/v1/projects` | [Projects](/docs/reference/projects) |
| Billing Profiles | `/admin-api/v1/billing_profiles` | [Billing Profiles](/docs/reference/billing-profiles) |
| Bills | `/admin-api/v1/bills` | [Bills](/docs/reference/bills) |
| Account Credits | `/admin-api/v1/account_credits` | [Account Credits](/docs/reference/account-credits) |
| Service Providers | `/admin-api/v1/service_providers` | [Service Providers](/docs/reference/service-providers) |
