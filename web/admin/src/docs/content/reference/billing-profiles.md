# Billing Profiles

Manage billing profiles and their lifecycle — activate, suspend, resume. Base path: `/admin-api/v1/billing_profiles`.

## The billing profile object

| Field | Type | Description |
|---|---|---|
| `id` | string | Billing profile id. |
| `organization_id` | string | Owning organization. |
| `status` | string | Lifecycle status. New profiles start as `NEW`; activation moves them to `ACTIVE`; suspension flows set suspended states. |
| `members` | array | Always present; exactly one entry `{ "sub": ... }` — the owning user. |
| `first_name` | string | Contact first name. |
| `last_name` | string | Contact last name. |
| `email` | string | Contact email. |
| `company` | boolean | Always present. `true` for company profiles. |
| `company_name` | string | Company name (omitted when empty). |
| `tax_number` | string | VAT / tax number (omitted when empty). |
| `address` | string | Street address. |
| `city` | string | City. |
| `zip_code` | string | Postal code. |
| `region` | string | Region / county. |
| `country` | string | Country. |
| `phone` | string | Phone number. |
| `currency` | string | Invoice currency, e.g. `EUR`. |
| `created_at` | string (RFC 3339) | Creation timestamp. |
| `updated_at` | string (RFC 3339) | Last update timestamp. |

## GET /admin-api/v1/billing_profiles

Lists billing profiles, paginated.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `organization_id` | string | no | Filter by owning organization. |
| `email` | string | no | Exact email match. |
| `member_sub` | string | no | Filter by the owning user's `sub`. |
| `limit` | integer | no | Page size (default 50, max 500). |
| `marker` | string | no | Keyset cursor. |

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  "https://<host>/admin-api/v1/billing_profiles?email=jane@example.com"
```

Response `200`:

```json
{
  "data": [
    {
      "id": "665f2a1bb8d34a0012340002",
      "organization_id": "665f2a1bb8d34a0012340001",
      "status": "ACTIVE",
      "members": [ { "sub": "user-9c4d1a2b3e4f5a6b7c8d9e0f1a2b3c4d" } ],
      "first_name": "Jane",
      "last_name": "Doe",
      "email": "jane@example.com",
      "company": true,
      "company_name": "Acme Corp",
      "tax_number": "EU123456789",
      "address": "1 Main Street",
      "city": "Amsterdam",
      "zip_code": "1011AB",
      "region": "Noord-Holland",
      "country": "NL",
      "phone": "+31123456789",
      "currency": "EUR",
      "created_at": "2026-06-01T09:30:00Z",
      "updated_at": "2026-06-15T12:00:00Z"
    }
  ]
}
```

## GET /admin-api/v1/billing_profiles/{id}

Fetches one billing profile. Response `200`: `{ "data": { ...profile } }`.

Errors: `404 NOT_FOUND`.

## POST /admin-api/v1/billing_profiles

Creates a billing profile. The new profile starts in status `NEW` and becomes the organization's billing profile.

| Field | Type | Required | Description |
|---|---|---|---|
| `organization_id` | string | no | If set, must exist. If omitted, an organization named `"<first_name> <last_name>"` is created automatically. |
| `members` | array | yes | **Exactly one** entry `{ "sub": ... }`; the `sub` must resolve to an existing user. |
| `first_name` | string | yes | Contact first name. |
| `last_name` | string | yes | Contact last name. |
| `email` | string | yes | Contact email. |
| `company` | boolean | no | Company profile flag. |
| `company_name` | string | no | Company name. |
| `tax_number` | string | no | VAT / tax number. |
| `address` | string | no | Street address. |
| `city` | string | no | City. |
| `zip_code` | string | no | Postal code. |
| `region` | string | no | Region / county. |
| `country` | string | no | Country. |
| `phone` | string | no | Phone number. |
| `currency` | string | no | Invoice currency. |

Response `200`: `{ "data": { ...profile } }`.

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Malformed body, or `Only 1 billing profile member is supported.` |
| `404 NOT_FOUND` | `Organization not found`, or `User not found` (member `sub`). |

## PUT /admin-api/v1/billing_profiles/{id}

Updates a billing profile. **Full replace:** the request body replaces the profile's contact/company fields; `id`, `status`, and `created_at` are preserved. The body uses the same fields (and the same member rule) as the create request.

Response `200`: `{ "data": { ...profile } }`.

Errors: `404 NOT_FOUND`, `400 BAD_REQUEST` (`Only 1 billing profile member is supported.`), `404 NOT_FOUND` (`User not found`).

## POST /admin-api/v1/billing_profiles/{id}/activate

Activates a `NEW` billing profile (completes the activation constraint, grants any configured sign-up credits, and enables the account). Activating a profile in any other status has no effect. No request body.

Response `200`: `{ "data": { ...profile } }`.

Errors:

| Status | Condition |
|---|---|
| `400 BAD_REQUEST` | Activation processing failed. |
| `404 NOT_FOUND` | Profile doesn't exist. |
| `501 NOT_IMPLEMENTED` | Billing activation isn't configured on this deployment. |

## POST /admin-api/v1/billing_profiles/{id}/suspend

Suspends the billing profile (API suspension source): the suspension orchestration pauses/disables the account's projects. No request body.

Response `200`: `{ "data": { ...profile } }`.

Errors: same as activate (`400`, `404`, `501` when not configured).

## POST /admin-api/v1/billing_profiles/{id}/resume

Lifts an API-source suspension and re-enables the account's projects. No request body.

Response `200`: `{ "data": { ...profile } }`.

Errors: same as activate (`400`, `404`, `501` when not configured).
