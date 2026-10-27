# 3. Authenticate as an OAuth2 / OIDC resource server (plus SigV4 for machines)

## Status

Accepted

## Context

ProxCloud serves three audiences with different trust characteristics:

- **customers** in the console SPA — human logins, needing SSO, MFA, password
  reset, social login, account self-service;
- **operators** in the admin SPA — human logins with elevated privilege;
- **machine clients** of the public admin API (`/admin-api/v1`) — automation with
  no browser and no interactive login.

Building and operating our own identity store (password hashing, credential
recovery, MFA, session management, token issuance, brute-force protection) is a
large, security-critical surface that is a poor use of effort for a cloud
platform whose value is elsewhere. We would rather delegate identity to a
hardened, standards-based system and have ProxCloud only *consume* the result.

## Decision

ProxCloud is an **OAuth2 / OIDC resource server**. It does not issue tokens,
manage credentials, or hold sessions. An external OpenID Provider (an instance of
**Keycloak** ships in the Helm chart for a turnkey deployment; any conformant
OIDC issuer works) authenticates users and mints JWTs; ProxCloud only **validates**
them.

- Human traffic carries `Authorization: Bearer <jwt>`. The authenticator
  (`pkg/auth`) verifies the token against the discovered per-realm OIDC
  verifiers and derives the caller's identity from standard claims (`sub`,
  `email`, `given_name`, `family_name`, `azp`). Realms are discovered in the
  background, so startup never blocks on an unreachable issuer; until a verifier
  is available, protected requests fail closed with `401` + `WWW-Authenticate:
  Bearer`.
- Identity is split into **realms** by audience (see `config.Auth`): a `clients`
  realm for the customer console, a separate realm/client for the operator admin
  console, and a realm/client for the public admin API. The SPAs are **public
  PKCE clients** (`pkce.enabled: true` in the chart values), so no client secret
  ships in browser code.
- **Machine clients** authenticate to `/admin-api/v1` with **AWS Signature
  Version 4**–style signed requests. Access-key pairs live in the `hmac_keys`
  table (`id = pk…`, `secretKey = sk…`); `pkg/auth/sigv4.go` recomputes the
  canonical-request signature and enforces a 5-minute clock-skew window. Keys are
  minted by an operator command, not self-service.
- A request context carries either the OIDC-derived principal or the SigV4 key
  id, so downstream policy is uniform regardless of scheme.

## Consequences

- We inherit SSO, MFA, password/credential flows, and social login from the IdP
  and never store customer passwords.
- A deployment **must** run and trust an OIDC issuer. The chart bundles Keycloak
  (with its own PostgreSQL) so the default install is self-contained; production
  can point at an existing IdP by setting `auth.*.issuer` to that issuer.
- Realm/client configuration is part of deployment config and must match on both
  the SPA (issuer, client id, PKCE) and the API (issuer, expected audience) or
  tokens are rejected.
- Two auth schemes coexist in one gate; both are validated in `pkg/auth` and
  both resolve to the same request-context shape, keeping handlers scheme-
  agnostic.
- Because ProxCloud never issues tokens, there is no local login endpoint to
  attack, but availability of auth is coupled to IdP availability (mitigated by
  background realm discovery and fail-closed behavior).
