# Validation approval outbox: safety and default-OFF operation

## What the event means

This family concerns **`billingProfileValidation`**, the business document updated
by `POST /api/v1/admin/billing-profile/validations/{validationId}/status/{status}`.
It is not the billing client's `identityValidation` document or KYC attachment.

The new fixed system-template key is `billing_profile_validation_approved`. Its
subject and body attest **validation approval only**. Approval does not imply an
ACTIVE account, successful promotional credits, provisioned projects, cloud
readiness, relay acceptance, or inbox delivery. The separately executed activation
tail may fail or no-op after the approval decision commits; the message remains
truthful. Nothing in the mail worker calls or retries activation.

The existing `billing_profile_validated` template and `NotifyValidation` are
unchanged. Startup seeds the new key create-if-absent, never rewrites the old key
or an operator's existing rows. Operators editing the new template must preserve
its approval-only meaning.

## Configuration and rollout boundaries

`PROXCLOUD_MAIL_VALIDATION_APPROVED_OUTBOX_ENABLED` is a strict boolean and defaults
to **false**. It is independent of the savings-reminder and project-invite flags.
ON requires the existing strict blob key (at least 32 bytes in
`PROXCLOUD_ENCRYPTION_DEFAULT_KEY`). OFF startup remains compatible with absent or
short legacy keys. Cancellation is always wired and needs no encryption key.

This is enqueue-only. The global production dispatcher remains disabled. There
is no new delivery flag, cron job, replay endpoint, or administrative resend API.
Do not infer authorization to enable delivery or use real recipients from this
implementation.

Upgrade **all readers and status writers before opt-in**, then confirm the new
seed and exact schema constraint. Older binaries know only one or two families
and cannot safely process the third; do not roll them back onto a three-family
database. Turning the producer OFF is a safe producer rollback, not a schema
rollback. Retain the payload key for any later authorized inspection/dispatch of
existing rows. Never print keys, recipients, decrypted bodies, or raw KYC data.

Startup always ensures the outbox. The ON adapter additionally prepares the
validation/profile, message-template, billing-configuration and SMTP-integration
tables before serving. The small decision helper ensures its two business stores
before entering its owned transaction, including on OFF/keyless deployments.
Missing optional tables must not be caught after aborting the decision transaction.

## Transaction and history rules

The helper rejects inherited transactions, including another DB's context. It
opens an owned PRIMARY transaction and sets READ COMMITTED **before the first
primary probe/SELECT**, even when the connection default is REPEATABLE READ.
The lock order is:

1. Actual `billingProfileValidation` row.
2. Its currently referenced `billingProfile` row.
3. Immutable outbox event and recipient rows.

The recipient snapshot comes only from that locked profile, not from a pre-lock
map. All status writers using this route take the validation lock, even OFF.
Workers lock outbox rows only and never take business-document locks.

| Locked decision | Mail behavior |
| --- | --- |
| Non-APPROVED to APPROVED, ON | Fresh UUID approval generation, DB-clock Date, fixed 24-hour expiry, one template/config snapshot, typed enqueue and status/history commit together. Missing referenced profile aborts before the status change. |
| Already APPROVED, durable | Original generation, Date and outbox IDs; no render, requeue, or new generation. |
| Already APPROVED, no marker, ON | Historical approval: no adoption, backfill, or legacy fallback. |
| PENDING or REJECTED with durable history | Cancel persisted unsent IDs in the same status transaction, including producer-OFF/keyless. Failure rolls back status and cancellation. |
| Reapproval, ON | Fresh generation only on a real nonapproved-to-APPROVED transition. Retain first-generation identity and prior-generation count; old immutable events/receipts remain in the outbox. |
| APPROVED, OFF, with durable history | Conservative status-only approval. Retain the revoked/history marker; no new mail generation, unencrypted decision, or legacy fallback. A later ON repeat of this already-approved status does not backfill it. |
| New never-durable operation, OFF | Preserve legacy best-effort activation/`NotifyValidation` behavior and errors. |

The sticky `approvalDelivery` document stores the mode, latest generation/date/
expiry/exact outbox IDs, first generation, prior-generation count and revocation
bit. Revocation never deletes this evidence. IDs are bounded to 20 per generation;
the cancellation call is sliced into batches of at most 20. Old generation rows
are immutable, not deleted or resealed. There is no new withdrawal/delete API.

Any render/key/SQL failure or panic rolls back the new status, delivery marker,
event and recipient rows together. No SMTP, API, financial, cloud or provisioning
callback runs in this transaction. **The commit occurs before `Activate`**. The
route preserves its activation error result (and the unwired 501) even when the
approval decision already committed. A repeated approval retains existing
activation-tail behavior; it does not create another durable email.

Cancellation fences pending/claimed/blocked/suppressed/no-recipient rows, making
pre-intent claims stale. DISPATCHING, UNKNOWN and accepted rows cannot be unsent;
the same exact in-flight attempt may later record its definitive receipt. UNKNOWN
is a hold, never an automatic replay candidate. Cancellation errors do not
partially apply a status change.

## Fixed sealed contract

`mailoutbox.ValidationApproved` and `EnqueueValidationTx` are the only new core
entry points. No exported generic kind/template/variables/transport/attachment
payload is introduced. The adapter can read the one fixed template, prepare a
snapshot, resolve DB/env configuration and enqueue; it has no Mailer, Activate,
project or cloud capability. `PrepareTemplateSnapshot` uses the already-loaded
row, not a second template lookup.

Event identity is SHA-256 of `validation-approved/v1`, persistent validation ID
and approval-generation UUID, separated by NUL bytes. It never contains email,
KYC values or other PII. Date comes from the database and expiry is exactly
**Date + 24 hours**. This fixed notification TTL is unrelated to arbitrary KYC
fields and says nothing about activation completion.

`billing.Recipients(profile)` supplies profile and billing-contact emails.
Addresses are syntax checked even for suppressed/blocked decisions; nonempty
malformed entries fail closed. Normalize domains and remove display names;
case-insensitive deduplication uses **first spelling wins**, matching the existing
billing helper. Preserve the local-part's case in the SMTP destination. Sort by
the case-insensitive normalized mailbox, then allocate opaque `recipient-00` etc.
ordinals. Public recipient keys hash these ordinals, not email addresses.
A truly empty set records an explicit `no_recipient` row. Each envelope has one
recipient and one RCPT; other recipients are never exposed in that wire message.

Subject, From, plain/HTML bodies, full name, ordinary login URL, normalized
recipient manifest and snapshot digest are all in AEAD-protected canonical
plaintext. Login URL comes only from fixed application configuration: HTTP(S), no
userinfo, query, fragment, or credential-bearing reset/invite token. No document,
KYC attachment or raw arbitrary variable map is copied into this family. New
request/DTO and inherited Envelope String/GoString/slog representations redact
sensitive values. Immutable input or recipient-set drift conflicts rather than
silently changing an event. Database metadata/status reads expose opaque IDs and
state only.

## Exact three-family migration and failure boundary

The controlled migration validates the original single-savings CHECK or the
existing named `mail_outbox_family_v1` actual type, validated bit and exact
expression. It chains original single-kind to v1 to the new exact
`mail_outbox_family_v2` (savings, invite, validation; all version 1) in **one**
transaction. Ensure is idempotent, sets RC before its first primary probe and
serializes via the existing schema advisory lock. Unexpected missing/weaker/
unvalidated/wrong-type checks roll back; mere constraint names are not trusted.

No table rewrite, reseal or weakening of immutable triggers occurs. Global
Version stays 1. Savings and Invite AAD, canonical plaintext and ciphertext are
unchanged. Unknown kind/version pairs are rejected before opening the sealed
payload. Validation's canonical payload, event, recipient and expiry must validate
before creating any dispatch intent. Cross-family or cross-row swaps fail closed.

## Local verification and explicit non-verification

The implementation was built offline with Go 1.25.14 (`GOTOOLCHAIN=local`,
`GOPROXY=off`, `GOSUMDB=off`) and a private cached PostgreSQL 17 loopback fixture,
with separate pools and default-REPEATABLE-READ competitors. Proofs cover atomic
error/panic/status-SQL rollback, invisibility, wrong DB/inherited rejection,
one-generation/one-template competition, duplicate no-rerender, history/OFF/no
fallback, fresh reapproval, missing profile/template, invalid recipients,
blocked/suppressed/no-recipient outcomes, cancellation and post-intent receipts,
fake activation after commit, v1-to-v2 original ciphertext/AAD/wire preservation,
concurrent Ensure, schema drift and zero-intent tampering.

Existing Savings and Invite PG regressions retain their assertions. Three
constraint-identifier occurrences in the Invite corruption fixtures were adapted
from the obsolete v1 name to current v2; archived original schema fixtures are
untouched. Actual old integration bodies run with a temporary **TestMain-only**
overlay against the owned loopback fixture; compile-only uses `go test -c` because
that package's ordinary TestMain starts Docker even for an empty test regex.

No real activation, promotional credits, financial posting, PVE/Kubernetes/cloud,
real recipients or external SMTP were tested. Existing loopback-only SMTP
capability tests are not a production delivery guarantee. Independent whole-suite
review, final merge and any qualified default-OFF deployment belong to the owner.
