# Savings contract-expired outbox: fourth family, default OFF

## Decision and deliberately conservative availability

This is the **contract-expired notice**, not the existing pre-expiry reminder.
`SavingsService.ExpireContracts` retains its supplied scheduler-cutoff semantics:
only a locked, currently ACTIVE contract with a valid EndDate **strictly before**
the cutoff becomes EXPIRED. That status influences future discount eligibility;
expiration does not post money or call payment, credit, charge, provisioning,
cloud or network callbacks.

Exactly one notice decision is permitted for a canonical term:

```text
canonicalEnd = EndDate.UTC().Truncate(time.Microsecond)
termGeneration = hex(SHA256(canonicalEnd.Format(time.RFC3339Nano)))
eventKey = hex(SHA256("savings-contract-expired/v1" + NUL + ContractID + NUL + termGeneration))
```

There is **no fresh lifecycle UUID**. Same-EndDate reactivation or reapproval does
not send again, even if a new lifecycle might legitimately deserve a notice.
This conservative availability limit is intentional. A different canonical
EndDate permits a new term; returning to any previously processed EndDate does
not. If notices per reactivation are ever required, that is a separately reviewed
**all-writer lifecycle epoch protocol**, not an extension hidden in this codec.

## C4: context and containers

```text
Operator / existing scheduler / existing debug expiry trigger
                  |
                  v
ProxCloud API process [existing expiry job; no new worker or job]
  billing.SavingsService ----> billing.Repo [owned DB-only decision]
                                  |
  fixed cmd/api adapter <----------+  current locked profile + Recipients(profile)
       |                          |
       +-- one messageTemplate DB snapshot: savings_contract_expired
       +-- fixed snapshot renderer (six existing template variables)
       +-- DB/env-only durable config resolver; no SMTP probe
       +-- typed mailoutbox.SavingsExpired preparation + AEAD
                                  |
                                  v
Owned PRIMARY PostgreSQL
  savingsContract / reminderNotification
  savings_expired_term [private, persistent, not a document collection]
  mail_outbox_event / mail_outbox / mail_outbox_attempt

SMTP dispatcher: NOT REGISTERED / OFF. No recipient or transport activation here.
```

The six template variables are `fullName`, `savingsPlanName`, `currency`,
`startDate`, `endDate`, and `monthlyCommittedAmount`. The request has no raw
variable map, caller-selected template/kind, URL, attachment, transport, job
function or SMTP settings. Existing mail shell/business-name rendering is
unchanged. The adapter has only fixed snapshot/render, template-read, config-read
and typed enqueue capabilities; no Notifier or activation/payment callback.

### C4: components and atomic sequence

1. Reject **any** inherited pgdoc transaction (same or different DB). Ensure
   prerequisites outside a business transaction; do not strip context or change
   someone else's snapshot. Actual admin/payment callers invoke the existing
   contract DB CAS outside gateway transactions.
2. Open the owned transaction. Its **first SQL statement** sets READ COMMITTED,
   before PRIMARY verification or advisory queries. This overrides a pool default
   of REPEATABLE READ without changing global pgdoc behavior.
3. Lock `savings-reminder:<contractID>`, then re-read/lock current contract, reminder
   rows, term history. Only then may DB-only callbacks touch outbox rows. The
   advisory also serializes a missing/deleted contract.
4. Recheck ACTIVE, valid EndDate, and strict EndDate < caller's cutoff. A stale
   candidate is merely an identity hint, not authorization to expire an old term.
5. Freeze decision Date using PostgreSQL `clock_timestamp()`, microsecond UTC;
   notification expiry is **Date + 24 hours**, not the already-past EndDate.
6. Cancel existing reminder decisions and old expired-notice generations. Flip
   EXPIRED. If this canonical term has any history, stop without another notice.
7. When opted in, read/lock the **current** billing profile, resolve its current
   billing recipients, obtain exactly one fixed template snapshot, render and
   enqueue the typed sealed snapshot. Persist its exact selected IDs and state.
8. Commit status + reminder cancellation + term selection + all outbox rows
   together. Missing profile/template, rendering/key/enqueue/cancel/status/history
   write errors or panics roll back every effect. Counts are expiration/queued
   decisions, never SMTP acceptance, delivery or inbox counts.
9. Only a winning first OFF decision may invoke the existing best-effort notifier,
   **after its legacy/no-mail history commits**. A crash there can lose the email;
   no later toggle or restart adopts it into durable delivery or redrives it.

A profile-row read uses `GetForUpdate` inside this transaction. Template/config
reads are DB/env-only; no SMTP, payment or cloud callback runs inside SQL.

## State and history model

The private `savings_expired_term` SQL sidecar is keyed by
`(contract_id, term_generation)`, not a field in public SavingsContract JSON.
It retains canonical source end, original opaque billing-profile identity,
original selection mode, DB decision date/expiry, exact outbox IDs and selected
queue state. It stores no names, addresses, raw variables, body, token or receipt.
There is no FK cascading from the deletable contract and no public history-edit,
receipt, resend, recovery or adoption endpoint.

| First selection | Meaning | Later same-term selection |
| --- | --- | --- |
| `durable` | Frozen outbox snapshot, including blocked/suppressed/no-recipient | Never |
| `legacy` | First OFF decision may attempt legacy notifier after commit | Never |
| `no_mail` | No notifier, or observed historical EXPIRED term | Never |
| Any selection + `cancelled_at` | Closed by a cooperative identity/status/end/delete mutation | Never |

Original selection columns are immutable. A trigger permits only the monotonic
NULL-to-timestamp `cancelled_at` change; updates to selected IDs/mode/profile/date
and deletion are denied. Full-replace of the public contract cannot erase this
history. Raw admin DTO fields cannot supply history or receipts.

Already EXPIRED historical rows are not scanned, adopted or backfilled. When a
cooperative admin mutation **observes** a historical EXPIRED term with valid old
EndDate and no sidecar, it first records a no-mail tombstone in that same
transaction. This prevents EXPIRED-to-ACTIVE same-EndDate reapproval from creating
a duplicate after upgrade. Failed edits roll back this observation too; a new
EndDate can still become a new term. This is evidence retention, not old-message
adoption.

### Complete cancellation map

All four mutation paths use the same owned boundary and cancel old selected IDs
on status, canonical EndDate or profile-identity change/deletion:

- Client cancel/extend through `MutateSavingsContract`.
- Raw admin update/delete through `WithSavingsContractMutation`; rendering-only
  and no-op edits retain the frozen selection.
- Upfront PENDING_PAYMENT-to-ACTIVE **DB CAS only**. Rebase dates using the locked
  current contract, preserve eligibility/amount guards and return winning current
  fields to the legacy caller. Gateway transport, credit and notifier remain in
  their unchanged caller, outside SQL.
- Boot ACTIVE-unpaid-to-PENDING_PAYMENT **DB CAS only**, with existing price/end
  guards, idempotent won result and locked-current arithmetic.

Cancellation remains wired keyless while the producer is OFF. Each call contains
at most 20 persisted IDs. It fences `pending`, `claimed`, and `blocked_config`
(and explicit suppressed/no-recipient rows) before intent. It never changes
DISPATCHING, UNKNOWN, accepted, dead, attempt identity or late exact receipts.
There is **no unsend guarantee** after committed intent. Fresh expiry selection
occurs *after* old-generation cancellation, so the EXPIRED status write does not
immediately cancel its own newly selected message.

## Typed payload and schema compatibility

The new fixed kind is `savings-contract-expired/v1`, version **1**. Core preparation
validates canonical generation and dates, exact 24-hour lifetime, bounded safe
text, strict addresses, maximum 20 manifest entries and a maximum 1 MiB wire
message. First spelling wins deduplication while preserving local-part case;
domains are normalized, sorting is deterministic and recipient metadata uses
opaque ordinal keys. Every envelope contains exactly one RCPT. Manifest, template
variables, body and From/To are AEAD-sealed; keyed immutable-event fingerprints
reject any same-event snapshot/recipient/date drift. DTO, request and envelope
String/GoString/slog values are redacted.

The private family AAD/new-case canonical decoder authenticate kind/version,
event, ordinal, expiry and exact canonical plaintext **before dispatch intent**.
Unknown pairs, cross-family swaps, modified ciphertext/metadata and unknown
canonical fields return no envelope and record zero intent. The existing three
families' AAD, canonical plaintext, ciphertext and wire bytes are unchanged, as is
global Version 1.

`Store.Ensure` holds its existing schema advisory under owned PRIMARY/RC and runs
the controlled single-kind -> family_v1 -> family_v2 -> **family_v3** chain.
Only the exact validated CHECK type/expression is accepted at each step. The new
v3 check adds only the fourth fixed kind/version pair; missing, weakened,
unvalidated, wrong-type or unexpected coexistence/drift fails the entire DDL
transaction. No table/data rewrite or resealing; immutable outbox triggers are
not weakened. Repeated/concurrent Ensure calls are supported.

## Rollout and rollback boundaries

- Dedicated `PROXCLOUD_MAIL_SAVINGS_EXPIRED_OUTBOX_ENABLED` is default **false**.
  Invalid boolean text fails configuration. ON requires the existing strong
  BlobKey material (at least 32 bytes); do not enable or rotate a short lab key.
- All replicas must understand v3 and the new payload **before** producer opt-in.
  Older binaries expecting v2 must not be blindly rolled back against v3.
- Startup ensures the private sidecar even OFF, without selecting any terms.
  Outbox cancellation remains configured independently of producer/key presence.
- No new dispatcher flag, delivery worker, scheduler, probe, transport or replay
  endpoint. Global SMTP dispatcher remains OFF. Disabling a producer is not
  permission to fall back from durable history to immediate legacy SMTP.
- Existing cron/debug expiry semantics remain; never use a live expiry trigger as
  a schema smoke test. Parent-owned deployment acceptance is default-OFF/schema
  only, not business expiry, mail, invite or validation activation.
- Frontends, deployment/workflows, go.mod, financial posting, create-only runtime
  holds, real GC and GPU are outside this feature.

## Bounded verification and fixture disclosure

Tests use local offline Go 1.25.14 and one owned cached PostgreSQL17 container,
literal loopback binding, tmpfs data and zero mounts. No actual SMTP, financial
posting, gateway, cloud or live expiry call is needed.

- Pure core/config/adapter tests and race tests: canonical UTC-microsecond identity,
  strict bound/address/wire validation, single RCPT, fixed single-snapshot adapter,
  dedicated OFF/keyless flag, redaction and immutable same-event drift.
- Billing owned-PG matrix (also raced): contenders/two pools, DB-clock timestamp,
  rollback/panic including reminder cancellation, OFF postcommit crash/no adoption,
  durable OFF no fallback, historical same-End ON/OFF suppression, new EndDate,
  failed edit, four writer paths, full-replace/delete/history, keyless cancellation,
  post-intent/UNKNOWN/accepted late ACK, inherited same/cross-DB rejection, and
  deterministic default-RR advisory-queue end/profile mutation barriers.
- Core owned-PG matrix (also raced): exact approved v2 source archived unchanged,
  all three populated old-family ciphertext/fingerprint/plaintext/AAD/wire bytes
  preserved across repeated/concurrent v3 Ensure, strict v2/v3 drift rollback,
  eleven tamper cases returning zero intent, exact retry and immutable drift.
- Existing savings/upfront DB, reminder, invite and validation assertions retained.
  Parent explicitly approved only current latest-family identifiers v2 -> v3 in
  the invite drift/tamper setup and validation latest-family assertion/tamper setup;
  historical v1/v2 drift fixtures remain unchanged. These are assertion-preserving
  current-schema adaptations, **not** claims of byte-identical old test bodies.
- Existing integration TestMain is compiled with `go test -c` when checking build
  only. Actual old regression execution uses an uncommitted TestMain-only Go
  overlay selecting the owned loopback DSN; freshPG and old assertion bodies are
  unchanged except the approved constraint identifiers. Reminder-generation's
  direct Testcontainers startup uses a separately approved fixture-only overlay;
  its assertion body is unchanged. No fallback pulls a Docker image.
- An initial new fixture `SHOW server_version_num` text-to-int scan failed before
  tests ran. Correcting it to `SELECT current_setting('server_version_num')::int`
  then rerunning the full matrix passed. No production guard was removed to pass.

Parent retains independent final security review, serial all-ten tests, GitHub
merge/tree equality and default-OFF schema acceptance. This branch pushes a PR
only; passing local tests does not authorize merge, delivery or activation.
Full Cloudflare audit remains **UNRUN**, late before GPU-last; this feature does
not run/install/upload it.
