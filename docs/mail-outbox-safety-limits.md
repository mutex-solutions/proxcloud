# Durable reminder mail outbox: safety and limits

## Status and non-goals

**PR A is dormant infrastructure, not a reminder rollout.** Startup creates the
primary tables and exposes read-only admin metadata. There is no production
worker registration, environment switch, business producer, cron, resend API or
backfill. Existing best-effort notification families and legacy SMTP/text
cryptography behavior are unchanged. A configured SMTP integration alone cannot
activate this dispatcher. Tests explicitly call `StartWithFakeTransport`; its
SMTP implementation permits only literal loopback IPs and `.invalid` fixture
recipients. Production adapter instances do not grant that test capability.

PR B must separately couple the savings reminder decision, generation, selected
window, enqueue and reminder bookkeeping in the same business transaction. It
must also handle cancellation/extension and legacy processed reminders. Invoice,
payment, activation, dunning and invitation notification rewiring is out of scope.

## Storage and transaction contract

- `internal/mailoutbox` owns `mail_outbox`, `mail_outbox_event`, and the append-only
  `mail_outbox_attempt` journal. It does **not** reuse DB-only durable job completion
  for SMTP. All ownership and receipt operations use the writable primary.
- `EnqueueTx` requires the exact owning `pgdoc.DB.WithTx` context. No transaction
  means rejection, not an independent enqueue. A wrong-DB context is rejected.
  Worker/status/schema boundaries reject **any** inherited transaction, so an
  intent cannot accidentally remain uncommitted while SMTP runs.
- The fixed allowlist is `savings-reminder/v1`, version 1. Event identity derives
  from contract ID, expiry generation and reminder-window days; the recipient
  key is opaque, not an address or reminder-document ID. There is no arbitrary
  destination/job/function HTTP API.
- Maximum **20 recipients per event**, one immutable envelope and one RCPT per
  recipient. Duplicate keys and duplicate normalized addresses are rejected.
  Domain names are lowercased; local-part case is preserved. The initial mailbox
  subset is ASCII dot-atom local parts with DNS-style domains, no SMTPUTF8,
  quoted-special local parts, address lists, CR/LF/control characters or domain
  literals. Display names are MIME encoded; bodies are base64 MIME parts.
- Frozen From, To, Date, subject, stable Message-ID and rendered text/HTML wire
  bytes are sealed before SQL insertion. There are no attachments or persisted
  template variables, gateway credentials, authentication tokens or raw SMTP
  errors. Each wire message is at most **1 MiB**, including headers/MIME encoding;
  exceeding it is an enqueue error, never silent truncation.
- A keyed fingerprint of canonical **plaintext** (including the recipient manifest
  and draft digest) rejects immutable changes. Randomized AEAD ciphertext is never
  compared for deduplication and never overwrites the original payload. Retrying
  the same enqueue returns original IDs/current state. New expiry means a new
  generation, not a payload edit. SQL triggers reject payload, event or journal
  mutation/deletion through the ordinary tables.

## Keys and privacy

The additive `textcrypt.BlobKeys` API is AES-256-GCM, format version 1, with a fresh
12-byte nonce and 16-byte authentication tag. It derives separate encryption and
fingerprint keys with domain-separated HMAC-SHA256. Key material must be at least
32 bytes; this length check is **not an entropy test**. Operators must use
cryptographically random secrets, not a 32-character human password. The existing
configured encryption secret is usable when it meets this contract. Legacy
`Encrypt`/`Decrypt` fail-open compatibility is unchanged and never used on outbox
payloads.

AAD authenticates the outbox ID, fixed kind, payload version, blob format version
and public key ID. The SQL blob is `bytea`; no plaintext fallback exists. Unknown
key ID, removed old key, wrong key/AAD/version or corruption fails closed before
intent or SMTP, yielding a blocked payload decision. No key/body/address is put
in status or logs. Startup currently uses public key ID `default-v1`; missing or
short legacy configuration logs a generic disabled-key warning, does not break
the existing inactive deployment, and cannot enqueue a payload.

The key-ring API accepts an active ID plus retained original keys, so old rows
can still be opened and deduplicated after rotation. There is deliberately no
rotation admin API/config rollout in this slice. Do not replace a key under an
existing ID or remove an old key with unresolved rows. A future rollout must
explicitly configure retained keys/IDs and reconcile missing-key holds.

## Dispatch, failures and bounds

1. **T1:** short DB-clock `SKIP LOCKED` claim with random UUID owner, increasing
   epoch and renewable bounded lease. Expired `claimed` work has no intent and may
   be rescued safely.
2. **T2:** lock the row, read a fresh database clock after any wait, authenticate
   the frozen blob, CAS `DISPATCHING`, append attempt-specific intent and perform
   another final fresh-TTL CAS **after** journal insertion. Commit before opening
   the socket. Expired intent creation rolls the whole transaction back.
3. **Network:** no SQL transaction is held. The adapter has context-aware dial,
   hard socket deadline and cancellation that closes the underlying socket.
4. **T3:** short transaction appends the typed safe receipt for the exact attempt.
   An ACK database failure leaves the intent uncertain, not eligible to resend.

States are `pending → claimed → DISPATCHING → accepted / pending / dead / UNKNOWN`.
`blocked_config`, `suppressed`, `no_recipient` and `expired` are explicit
**non-delivery**, not acceptance. Known-unsent temporary failures retry with
exponential positive jitter, at most **8 intents** (2-second first upper bound,
256-second last upper bound). Age is at most **30 days from the frozen decision
Date**, and never beyond frozen contract EndDate. At either bound work expires;
these are preparation/transport limits, not business reminder-window selection.
There is no automatic release of blocked rows in this slice.

Expired `DISPATCHING` becomes durably `UNKNOWN`, never pending: an old SMTP socket
may still be alive. UUID/epoch fences protect SQL, **not SMTP**. There is no
force-clear/replay/resend API. A late definitive receipt may settle the **same**
UNKNOWN attempt only while its owner/epoch/attempt identity is unchanged; an old
attempt cannot update a newer attempt. UNKNOWN may be an indefinite availability
hold requiring manual investigation. A future duplicate-risk resend requires
separate authorization, positive old-worker quiescence and immutable audit.

## SMTP acceptance boundary

| Evidence | Receipt / policy |
|---|---|
| No gateway, nil/Noop legacy path, TLS unavailable | Blocked configuration; never accepted |
| Before DATA or body write fails **before EOD is attempted** | Proven not accepted; retry only transient failure |
| Explicit SMTP 4xx / 5xx negative reply | Known not accepted; 4xx bounded retry, 5xx permanent/dead |
| EOD possibly written, but no definitive final reply | UNKNOWN, no automatic retry |
| DATA final `250` observed, then QUIT fails | Accepted by relay; QUIT cannot reverse acceptance |
| Untyped custom sender returns error after entry | UNKNOWN; error alone cannot prove unsent |

The adapter splits dot-stuffed body bytes from the final `.\r\n` write. It does
not infer non-acceptance from a failed terminator write or EOF after it. One RCPT
per envelope prevents retrying an accepted recipient due to a different refused
recipient. RCPT 25x responses are accepted. PLAIN and LOGIN are supported after
TLS; outside the explicit loopback test adapter, STARTTLS is mandatory (implicit
TLS on port 465), with certificate validation and no downgrade fallback. DB
integration configuration is resolved afresh per attempt and wins over env;
a present but broken DB gateway blocks, rather than silently selecting env.
Frozen From is not replaced by newly resolved gateway configuration.

**Relay accepted is not inbox delivered.** Stable Message-ID helps correlation;
SMTP relays and recipients do not promise exactly-once delivery from it.

## Read-only operations and evidence

`GET /api/v1/admin/mail-outbox` and `GET /api/v1/admin/mail-outbox/{opaque-uuid}`
require existing primary admin RBAC `admin:bill:read` before querying metadata.
Responses contain state counts, fixed reasons, attempts, opaque IDs and times,
never recipient/subject/body/blob/key/fingerprint/SMTP reply. Dispatch is reported
false. Status uses the existing primary only; no new replica connection/pool.
There are no mutation routes.

Targeted local tests use Docker PostgreSQL and loopback SMTP only. They cover
same-business rollback/invisibility, every wrong-DB boundary, competing workers,
claim rescue, intent crash/TTL holds, old live socket with no second RCPT/EOD,
late same-attempt receipt, old-attempt rejection, post-journal lock wait expiry,
relay acceptance followed by DB ACK failure, immutable fingerprint conflicts,
key rotation/missing key/tamper/AAD, bounded preparation and opaque RBAC status.
SMTP server transcripts distinguish pre-DATA refusal, pre-EOD EOF, consumed EOD
with lost `250`, `250` then QUIT error, and TLS/auth/cancellation ordering. Tests
observe actual protocol/SQL lock events, not timer guesses. No lab, configured
relay, real recipient, deployment or whole-project CI is exercised by this PR.
