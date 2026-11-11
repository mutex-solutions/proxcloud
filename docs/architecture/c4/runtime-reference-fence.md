# Runtime image reference fence (bounded P2-B engine)

## Status and boundary

This is a **dormant database engine**, not runtime caller integration or permission
to prune. Native and E2B create/resume/fork do not use this proof yet. The existing
scheduled/global registry deletion path **and** project-teardown deletion path
remain unconditional HOLD. No deletion route, operator clear, TTL cleanup, or
coverage switch is added.

P2-A BuildProof and import ImportProof remain distinct producer protocols. Runtime
is a consumer of one ready managed immutable image: it must protect that image for
the whole running incarnation, not release it when startup succeeds.

## C4 context and containers

- **Future trusted application caller** owns the current billing/runtime owner row,
  stable operation UUID, fresh immutable spec nonce and external dispatch CAS.
- **Registry pin Store** is a DB-only library with one trusted canonical registry
  host and one writable PostgreSQL PRIMARY. There are no network callbacks.
- **PostgreSQL** persists the single reference receipt and the one-shot
  `registry_runtime_dispatch` marker, independent of application transactions.
- **External runtime controller / CR / pod** is outside this engine. A future
  caller must independently read actual pod and current owner/spec provenance.

```text
Caller owner CAS -> BeginPins(exact immutable image) -> AuthorizeRuntimeDispatch
  -> committed marker -> Consume(full frozen spec) -> sole external create/apply
  -> independent pod + CR observation -> RecordRuntimePod -> ObserveRuntimePod
  -> image remains BOUND throughout Running / startup Ready
  -> excluded old Apply/controller + retired desired owner generation
  -> independent exact positive terminal pod -> RecordRuntimePod
  -> ReleaseRuntimeTerminal -> RELEASED receipt
```

The final exclusion/retirement condition is a **caller prerequisite**, not a claim
that this engine implements it. A Succeeded/Failed pod can otherwise be recreated
by a still-desired CR. Pod phase alone is necessary but not sufficient for a safe
whole-product release; future callers must retain BOUND/UNKNOWN until that desired
owner generation is retired and old external effects/recreation are excluded.

## Public contract

- `RuntimeSpec{ServiceID, SandboxID string; Mode RuntimeMode; Image, Fingerprint string}`
- `RuntimeCreate`, `RuntimeResume`, `RuntimeFork` (`create`, `resume`, `fork`).
- `RuntimePodEvidence{UID, CRUID, Operation, Nonce string; Spec RuntimeSpec; Phase string}`
- `AuthorizeRuntimeDispatch(ctx, op, spec) (*RuntimeProof, error)`
- `proof.Consume(project, spec) (operation, nonce string, err error)`
- `CancelUnusedRuntimeDispatch(ctx, proof) error`
- `RecordRuntimePod(ctx, op, evidence) error`
- `RuntimeTerminalKnown(ctx, op) (bool, error)`
- `ObserveRuntimePod(ctx, pin, evidence) (Pin, error)`
- `ReleaseRuntimeTerminal(ctx, pin, evidence) (Pin, error)`

The image must literally be `canonical-host/t/current-project/20hex-template@sha256:64hex`.
No tags, combined tag/digest, URLs, foreign namespaces or alternative host spellings
are accepted. Builtin/library/external non-deletable images may eventually skip
outside the engine; there is no low-level skip or fabricated proof here. Service
IDs use lower-case identity-safe characters; sandbox IDs are the real `sbx-` eight
or sixteen lower-hex forms. Fingerprint is the caller-frozen SHA-256 of its entire
canonical runtime spec, not a hash invented from the subset here. Raw spec,
credentials and environment payloads are never stored in the marker.

## Transaction and identity fence

Authorization rejects **any** inherited pgdoc transaction, including another DB's
transaction; it never strips context. Transactions set READ COMMITTED before the
first probe, require a writable PRIMARY, and acquire locks in this order:

1. trusted host namespace;
2. global operation UUID (shared by Build, Import and Runtime authorizers);
3. current project row (ENABLED or DISABLED, never missing/deleting);
4. exact dispatch/receipt rows.

The runtime factory inspects **all** receipts for the operation. Exactly one must
be pending, unbound, current owner/project/generation/host, and still equal to its
requested immutable image. The marker freezes that receipt ID/current epoch and
all spec fields. Global cross-kind marker checks run under the operation lock,
including across different host namespaces and projects. Existing producer public
interfaces, tags and binding semantics do not change.

Every pin advance composes Build, Import **then Runtime** guards, even if earlier
markers are absent. Private typed evidence is not a public grant boolean/callback.
The runtime guard requires its frozen receipt epoch and updates that epoch in the
same transaction as the complete pin CAS. Generic ObserveBinding, ReleaseBinding,
Build/Import observation or a copied matching receipt cannot bind or release a
runtime pin. Runtime evidence also cannot mutate a non-runtime operation.

## Proof and observation lifecycle

`RuntimeProof` has no public constructor. All value copies share one in-memory
atomic use/cancel bit. Consume is intentionally DB/network-free and compares the
project and **every** frozen spec field before using that bit. It is **not** a
durable used-bit. A trusted caller must consume before external dispatch; it must
not fabricate observation evidence from a receipt or call Record before Consume.
The durable operation primary key prevents remint/reissue from another Store or
after restart. A lost proof retains the hold; it does not grant redispatch.

Cancel invalidates that shared bit **before** any DB attempt. Only an exact unused,
never-observed authorized marker can become known_no_effect. The existing
ReleaseKnownNoEffect then releases its pending receipt. Used/lost proofs and DB
ambiguity cannot cancel; failed/ambiguous cancellation burns the proof and retains
the nonexpiring reference.

RecordRuntimePod separately commits independently observed pod UID, CR UID,
operation/spec nonce, literal image and entire frozen envelope. Only actual pod
Running/Succeeded/Failed are accepted. CR ReadyCandidate, missing CR/pod, deletion
204, timeout, lifetime expiry and Unknown are not positive terminal evidence.
ObserveRuntimePod can transition PENDING to BOUND while Running, unlike producer
destinations; BindingUID is the **pod UID**, BindingGeneration the spec nonce.
The CR UID is frozen separately because a resume can reuse a CR with a new nonce.

Positive terminal phase is monotone. Same-envelope stale Running after terminal
is a marker no-op, not permission to revive/rebind or release. Different UID,
nonce, CR UID, spec, image or terminal outcome conflicts. RuntimeTerminalKnown is
only a monotonicity query, not release authority. MarkUnknown updates the runtime
marker with the receipt and cannot downgrade known terminal/no-effect state.
UNKNOWN cannot be reopened even by late positive evidence and never expires.

## What remains before real GC

The engine provides **cooperative** DB exclusion only. Remaining P2-B work includes
native and E2B caller admission/owner CAS and low-level dispatch proof plumbing,
consumer lifetime/CR retirement and controller exclusion, snapshot/base/destination
writers, pause/paused image ownership, catalog/admin/provider writers and image
replacement hooks. Legacy full PRIMARY DB + live CR/pod/image provenance adoption,
old replicas/raw SQL/bypass writers, all-portal coverage and separately reviewed
exact GC intent execution are still prerequisites. There is no broad cooperative
all-writer guarantee, automatic recovery or claim of complete GC safety in this PR.
