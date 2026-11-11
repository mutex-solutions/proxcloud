# SSO alias retirement and retained sessions

Deleting an organization SSO provider must not make its still-issued brokered
sessions acceptable when it was the platform's last configured provider. The
resource server does not assume that deleting a Keycloak broker revokes its
access tokens, refresh tokens or sessions.

## C1 — Identity and revocation boundary

Only an alias taken from a stored ProxCloud provider is retired. A request token's
arbitrary `idp` string is never sufficient to create a retirement. Unknown aliases
and alias prefixes are not a denylist: the realm-level `identity_provider` mapper
also names legitimate operator-added social/directory brokers. With no configured
organization providers, those brokers and native local sessions keep their prior
behavior. The existing nonempty-provider unknown-alias rule is unchanged.

## C2 — Durable state and existing components

```text
Organization role manager -> DELETE organization SSO
  -> PRIMARY PostgreSQL: idempotent org_sso_retired_alias[stored alias] = {}
  -> invalidate this API process's rule snapshot
  -> Keycloak broker deletion
  -> release user SSO binding -> delete provider configuration -> audit

Retained brokered session -> API authentication -> SSO Guard
  -> primary-backed rule snapshot -> exact retired alias? refuse sso_idp_disabled
  -> only then consider no-org-providers fast path and existing domain/binding policy
```

`org_sso_retired_alias` is additive document storage with the alias as unique
primary key and an empty payload. No client secret, token, private key or provider
configuration is copied into it. No automatic expiry or pruning is used: the realm's
refresh/session lifetime is not guessed. The id-only rows and cached map grow with
provider deletions, not login requests. Alias allocation checks retirement before
reserving a new provider; deleted aliases are never deliberately reused.

## C3 — Service responsibilities

- `Repo.retireAlias` rejects inherited transactions so the durable fence commits
  before the caller starts any external deletion. Retries are idempotent.
- `Service.Delete` writes retirement first, invalidates its local cache even if
  that write returns an uncertain/error result, then performs existing cleanup.
  If the primary write fails, no external deletion is attempted; the service cannot
  promise durable revocation when no retirement committed.
- The snapshot loads retirement rows through the existing primary-only pgdoc pool.
  Retired providers are excluded from discovery, enforcement and reconciliation;
  an incomplete deletion cannot re-enable a provider by merely leaving its document.
- `Guard` checks exact retired aliases before the zero-provider shortcut. It does
  not reject every unknown broker or classify a provider by a token-controlled prefix.
- Draft/configuration and operator enablement refuse a deletion-in-progress alias.
  JIT provisioning additionally reads the retirement fence fresh before membership
  work, rather than treating a previously accepted guard snapshot as new authority.

## C4 — Failure and retry semantics

A committed retirement survives Keycloak failures, user-binding cleanup failure,
provider deletion failure, process restart and successful retry. An authorized
**non-retired** native/local break-glass session can retry the existing DELETE API
for that organization. The same retired brokered session is intentionally refused
and cannot be relied on to finish its own cleanup. A full UI reload may encounter
409 on the draft/configuration GET while deletion is pending; use the explicit
API/operator recovery path, not a new configuration of the retired alias.

Verified domains remain claimed, and existing organization/project memberships are
not removed by this change. A newly configured provider gets a different alias.
This is broker-session refusal, not user deletion or a claim of global IdP logout.

### Cache and rollout limits

The existing rule snapshot refreshes within one minute **while primary reads
succeed**. Other replicas may briefly use the older snapshot. The existing
stale-rule fallback during database failure is retained: an already warm process
can keep old rules beyond that minute until reads recover. A fresh or locally
invalidated process has no snapshot fallback and refuses unavailable rule data.
This patch does not claim a hard one-minute revocation SLA during primary outage.

Upgrade **all** API replicas before relying on retirement: old binaries do not
honor the additive rows. Preserve the table in backups/restore and do not roll
back readers to a version that ignores it after retirements are issued. Aliases
already deleted before deployment cannot be inferred or automatically backfilled.
No live identities or historical provider state are repaired by this source change.

A previously started concurrent configuration/JIT operation is not made globally
linearizable with deletion. A stale provider row saved later still cannot defeat
retirement when the guard snapshot reads the committed fence, but this is not a
new cross-operation locking protocol or a fix for the separate provider-save
lost-update lead.

## Regression and acceptance scope

Unit tests check exact retired-alias refusal before empty-provider success, native
local/social positives, operator-own-realm/HMAC exemptions and unchanged operator
broker refusal. Native private PostgreSQL tests exercise id-only retirement rows,
fresh-service restart, new alias allocation, fake Keycloak failure, injected user
and provider cleanup errors, idempotent retry, fresh rule-data failure, inherited
transaction refusal and stale-snapshot JIT containment. The fake Keycloak handler
observes the primary fence and local refusal before accepting DELETE.

These are isolated native repository/service tests, not a real Keycloak token
mint/revoke canary or a production rollout. No credential, realm mapping, mail,
cloud workload, payment or scheduler setting is changed by the test procedure.
