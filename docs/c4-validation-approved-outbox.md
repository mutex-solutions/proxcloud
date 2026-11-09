# C4: validation approval notification boundary

## Context (C1)

An authorized administrator approves a **billing-profile validation**, not an
account or cloud-service activation. ProxCloud persists the administrative
approval and an optional frozen approval-only mail decision in its PostgreSQL
primary. An independent, currently disabled dispatcher could later attempt relay
submission; neither an approval commit nor a relay receipt proves inbox delivery.

## Containers (C2)

```text
Administrator
  │ POST /api/v1/admin/billing-profile/validations/{id}/status/{status}
  ▼
API container
  ├─ Existing auth/admin permission boundary (unchanged)
  ├─ Admin-owned validation decision helper
  │     │ owned PRIMARY READ COMMITTED transaction
  │     ▼
  │   PostgreSQL primary
  │     billingProfileValidation → billingProfile → mail_outbox_event/rows
  │     status + sticky generation/history + immutable sealed snapshots
  │
  └─ AFTER COMMIT ONLY: existing Activate(SourceValidation) tail
        ├─ may no-op or fail
        └─ promotional-credit/project/cloud effects are OUTSIDE mail atomicity

Dormant production dispatcher ── no registration/enabling change in this family
  └─ future attempt/receipt protocol → SMTP relay (not inbox-delivery proof)
```

## Components (C3)

| Component | Responsibility and capability limit |
| --- | --- |
| Admin route | Preserve status enum/permission/errors; call owned decision, then activation tail. Legacy notifier only for OFF and never-durable decisions. |
| `decideValidationStatus` | Reject inherited TX; RC before probe; lock actual validation then referenced profile; fresh approval-generation/date or original decision; atomic revocation and sticky history. No sender/activation capability used. |
| Typed API adapter | Read `billing_profile_validation_approved` exactly once; `PrepareTemplateSnapshot`; DB/env-only `DurableConfigResolver`; fixed configured ordinary login URL; `EnqueueValidationTx`. |
| Typed core | Fixed `validation-approved/v1`, Version 1, bounded 24-hour expiry, strict addresses, dedup/opaque ordinals, one-recipient MIME, canonical sealed snapshot and immutable event fingerprint. |
| Private payload switch | Exactly three kind/version/AAD cases; unchanged savings/invite bytes; authenticate canonical validation metadata before dispatch intent. |
| Outbox schema/store | Owned primary transactions, exact validated v1-to-v2 migration, advisory schema lock, immutable event/payload/attempt journal, pending cancellation and late exact receipts. |
| Existing activation service | Postcommit account/provisioning business behavior only; neither adapter nor worker invokes it. Source is unchanged by this family. |

## Dynamic view (C4)

```text
status route     decision helper       validation/profile DB      adapter/core
    │                   │                       │                     │
    │── decide ────────► │ reject inherited TX   │                     │
    │                   │ BEGIN; SET RC         │                     │
    │                   │── primary probe ─────►│                     │
    │                   │── lock validation ───►│                     │
    │                   │◄─ current association/status/history ──────│
    │                   │── lock profile ──────►│                     │
    │                   │◄─ locked recipient/name snapshot ──────────│
    │                   │ DB Date; fresh UUID  │                     │
    │                   │── fixed typed request ─────────────────────►│
    │                   │                       │◄─ one template/config read
    │                   │                       │◄─ AEAD event+envelopes
    │                   │── status+sticky marker ►│                   │
    │                   │ COMMIT                │                     │
    │◄─ durable decision│                       │                     │
    │── Activate(SourceValidation), outside SQL │                     │
    │── skip legacy Notify for ON or durable history                  │
```

Any failure/panic before commit rolls the status, marker and envelopes back.
An activation failure **after** commit does not retract a truthful approval-only
notification. Repeated already-approved durable requests retain the original
generation/snapshot without re-rendering. Historical approved rows are never
adopted. Revocation locks the same validation row and cancels stored pre-intent
IDs even OFF/keyless; post-intent/UNKNOWN/accepted attempts remain evidence, not
"unsent" mail. OFF reapproval with durable history is status-only and never falls
back to the old fully-activated template.

For rollout rules, exact history/expiry/recipient semantics and verification
limits, see the [safety guide](./validation-approved-outbox.md).
