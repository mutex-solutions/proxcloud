# Authentication

The Admin API authenticates each request with **AWS Signature Version 4** (SigV4) — the same request-signing scheme AWS services use — computed from a ProxCloud HMAC key pair. No AWS account is involved; only the signing algorithm is borrowed.

## 1. Create an HMAC key pair

In the admin UI, open **System → API keys** and click **Generate key**. You get back:

| Field | Format | Description |
|---|---|---|
| `id` | `pk` + 32 hex chars | The access key id (also the document `_id` in the `hmac_keys` collection). |
| `secretKey` | `sk` + 40 hex chars | The signing secret. **Shown in full only once, at generation time** — store it right away. List/get reads never return it in full again. |
| `description` | string | An optional free-text label. |

Operators can also mint a pair on the management port: `POST /debug/gen-hmac-key?description=...` returns `{ "id": "pk...", "secretKey": "sk..." }`.

## 2. Sign your requests

Sign each request per the AWS SigV4 spec, with:

- **Algorithm:** `AWS4-HMAC-SHA256`
- **Access key / secret:** your `pk…` / `sk…` pair.
- **Region / service:** the verifier recomputes the signature from the credential scope in your `Authorization` header, so any consistent pair works. By convention use `us-east-1` / `execute-api`.
- **Signed headers:** at least `host;x-amz-date`. The `X-Amz-Date` header is required (`YYYYMMDD'T'HHMMSS'Z'`) and must sit within **±5 minutes** of server time.
- **Payload:** the request body is folded into the signature (a hash of the exact bytes sent).

`curl` supports SigV4 out of the box:

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk1f4d5e6a7b8c9d0e1f2a3b4c5d6e7f8a:sk0123456789abcdef0123456789abcdef01234567" \
  "https://<host>/admin-api/v1/organizations?limit=5"
```

For a signed POST:

```bash
curl --aws-sigv4 "aws:amz:us-east-1:execute-api" \
  --user "pk<access-key-id>:sk<secret-key>" \
  -H "Content-Type: application/json" \
  -d '{"email":"jane@example.com"}' \
  "https://<host>/admin-api/v1/users"
```

The resulting `Authorization` header looks like:

```
AWS4-HMAC-SHA256 Credential=pk.../20260703/us-east-1/execute-api/aws4_request, SignedHeaders=host;x-amz-date, Signature=<hex>
```

## The OIDC alternative

A bearer token from the dedicated **admin-api** OIDC realm is also accepted: the token's issuer and authorized party (`azp`) both have to match the configured admin-api realm and client. Tokens from any other realm — including the regular admin console realm — are rejected with `403`.

## Common causes of 401 / 403

| Status | Cause |
|---|---|
| `401` | No `Authorization` header, or one that is neither `Bearer …` nor `AWS4-HMAC-SHA256 …`. |
| `401` | Unknown access key id (`pk…` not present in the `hmac_keys` collection). |
| `401` | Signature mismatch: wrong secret, the body/URL/headers changed after signing, or the signed headers differ from what was sent. |
| `401` | `X-Amz-Date` missing, malformed, or more than 5 minutes off server time (clock skew). |
| `401` | The region/service in the credential scope differs from what was used to compute the signature. |
| `403` | A valid bearer token, but from the wrong realm or client (`issuer`/`azp` don't match the admin-api realm). |

Both `401` and `403` come back with an **empty body** — no JSON error envelope.
