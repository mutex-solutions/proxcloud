# Dormant snapshot registry dispatch core

## Scope: a prerequisite, not a snapshot producer

This change adds only the typed `registrypins.SnapshotProof` database protocol.
There is **no production caller**, feature flag, new worker, network client or
snapshot/pause/resume/fork integration. Existing registry schema Ensure can create
an empty `registry_snapshot_dispatch` table. That table does not enable capture,
publication, managed saved-runtime starts or physical garbage collection.

Both real manifest DELETE paths remain unconditional HOLD. Legacy adoption,
complete writer participation, desired-owner retirement, exclusion of outstanding
old Apply/controller recreation, and exact physical GC are separate prerequisites.
Existing runtime/source BOUND or UNKNOWN holds are not released by this protocol.

## C4 context and components

```text
Future trusted application snapshot owner [NOT implemented here]
   | independently establishes current source CR/Pod + provider revision
   | freezes exact fixed assembly image/command/environment request
   | commits BASE/DEST pins before external effects
   v
registrypins.Store -- owned PRIMARY PostgreSQL, READ COMMITTED FIRST
   | namespace -> global operation UUID -> current project -> marker/pins
   +-- registry_pin                     [existing full-row CAS receipts]
   +-- registry_snapshot_dispatch       [new fixed snapshot/v1 correlation]
   +-- registry_build/import/runtime_dispatch [mutual UUID exclusion]

Opaque SnapshotProof.Consume(project, exactSnapshotSpec)
   | DB/network-free shared single-use bit
   v
Future cloud observer / immutable publication owner [NOT implemented here]
   | actual assembly UID/spec/phase, not copied markers
   +-- RecordSnapshotPod / ObserveSnapshotPod
   +-- BindSnapshotDestination after separately COMMITTED publication
   +-- ReleaseSnapshotTerminal: managed BASE only

Registry/Kubernetes/object storage/SMTP/GPU: NO I/O from this package.
```

The new API is distinct from BUILD, content-preserving IMPORT, and runtime
consumers. An append-style snapshot output may have a different digest from BASE.
No existing import operation is reinterpreted as an append action.

## Fixed envelope and trust boundary

`SnapshotSpec` freezes exactly these credential-free strings:

- `ServiceID`, `SandboxID`, `SourceCRUID`, `SourcePodUID`;
- `SourceFingerprint`, `ProviderRevisionHash`, `ActionSpecFingerprint`;
- `Base`, `Destination`.

The three aggregates must be lowercase SHA256 hex. Service/sandbox/source identities
use the existing strict core grammars. The action is always `snapshot/v1`, never a
caller-selected mode. There is no raw CR, environment, token, credential, LayerURL
or URL-bearing request in the marker. Spec/proof/evidence String, GoString and slog
formats are redacted, including shallow proof copies.

**Correlation is not source authority.** Frozen UID/hash stamps do not prove that
the capture actually used a current source or that provider credentials/policy are
still current. A future trusted caller must independently establish the actual
current source CR/Pod and provider revision before dispatch. A future observer must
inspect the actual assembly UID, fixed action/image/command/env, BASE/DEST and
source correlation stamps. Copied annotations alone grant nothing. This dormant
DB protocol cannot manufacture capture, registry contents or publication truth.

### Reference representation

BASE is an exact canonical credential-free SHA256 digest, never a mutable tag or
combined tag@digest. An own managed `host/t/project/template@sha256:...` BASE needs
its own independent pending pin. A positively classified same-host `library/...`
or canonical external immutable BASE needs only DEST's pin. Every other same-host
non-library spelling must pass the own managed grammar; malformed, foreign,
unknown namespace, URL/userinfo/query/fragment/escape/whitespace/default-port or
noncanonical-host spellings cannot fall through as external.

DEST is an exact canonical own-project managed repository plus unresolved tag. It
always has its own pending pin. This is representation validation only: external
origin allowlists, pull credentials, and publish authorization remain the future
trusted caller's policy, not this core's approval.

## Authorize, consume and cancel

1. The caller first commits independent exact required pins with BeginPins. Pins
   must precede tag resolution, an owner/project/rotation transaction or workload.
2. `AuthorizeSnapshotDispatch` rejects inherited same-DB and cross-DB transactions.
   Its owned transaction sets READ COMMITTED before the first snapshot/probe, then
   checks PRIMARY and locks host namespace, global operation UUID and current
   project. It selects **all** operation receipts, not a filtered owner subset.
3. Project/owner/generation/host/refs/state/UID/binding-generation/epochs must match
   the exact required pending unbound set. The marker stores both role IDs and
   their current epochs. Missing, extra, foreign or already-bound receipts fail.
4. BUILD, IMPORT, RUNTIME and SNAPSHOT factories mutually exclude the same UUID
   across trusted hosts. Prior authorized, observed, terminal, known-no-effect or
   UNKNOWN history never grants identity reuse.
5. Only after the marker commits is an opaque proof returned. Consume compares
   project and every frozen spec field without DB/network I/O. A mismatch does not
   burn it; a successful consume burns every shallow copy once.
6. `CancelUnusedSnapshotDispatch` burns the shared proof bit **before** its DB CAS.
   Only the exact authorized/unobserved marker becomes known_no_effect. If the
   database fails, copies stay burned and nonexpiring holds remain. A lost proof
   cannot be recreated, read back, renewed or silently replayed.

## Assembly observation and lifecycle

```text
authorized -- exact first actual UID/Spec Pending or Running --> observed
     |                         |                                    |
     | unused proof cancel     +-- exact Succeeded / Failed ---------+
     v                                                              v
known_no_effect                                                  terminal
     |                                                     Succeeded or Failed
     +-- exact pending unbound pins may release                 |
                                                               +-- BASE terminal release
                                                               +-- Succeeded DEST typed bind

authorized / observed -- exact current pin MarkUnknown --> UNKNOWN (permanent)
```

Only Pending, Running, Succeeded and Failed are accepted assembly phases. NotFound,
timeout, Unknown, Ready or an empty phase is not terminal evidence. The first
assembly UID and full spec are immutable. Same-envelope stale Running after a
committed terminal is a no-op for recording; conflicting UID/spec/terminal fails.
UNKNOWN never reopens after later positive observations or in another Store.
`SnapshotTerminalKnown` only prevents stale error handlers poisoning a committed
terminal/no-effect result; it is not digest, release or replay permission.

`ObserveSnapshotPod` attaches the exact recorded assembly UID/nonce while keeping
BASE/DEST pending and their original refs intact. It does not bind a result digest.
Every advance composes the snapshot guard even if all other action markers are
absent, then performs the existing complete original-row CAS. The role epoch and
marker update roll back if a forged/stale receipt loses that CAS.

### Conservative release and handoff policy

| Role / condition | Allowed here | Explicitly forbidden |
|---|---|---|
| Exact unused canceled proof, unobserved pending roles | ReleaseKnownNoEffect | Cancel after Consume/observation or infer no effect from an error |
| Previously observed managed BASE + matching recorded Succeeded/Failed | ReleaseSnapshotTerminal | Absence, timeout, phase-only claim, wrong UID/nonce/spec or UNKNOWN release |
| DEST + recorded Succeeded + previously observed exact DEST receipt | BindSnapshotDestination with independently observed **already committed** durable publication binding | Generic ObserveBinding, preterminal bind, other repository, tag result, changed owner/digest |
| Failed DEST | Retain nonexpiring pending hold | Inferring Failed means no push/publication; terminal release |
| Successful BOUND DEST | Retain BOUND indefinitely in this slice | Generic ReleaseBinding, known-no-effect or terminal retirement shortcut |

The typed destination API requires exact recorded assembly evidence, an immutable
result in the same own repository, durable owner UID and matching generation. The
first destination digest/owner identity is stored in the marker and cannot be
replaced. Result digest need not equal BASE: snapshot append changes content.
The caller must have independently observed and committed immutable template
publication first; comparing a supplied binding is not proof of another collection
or registry state. There is intentionally **no DEST retirement API** in this slice.

BASE release neither drops paused/source snapshots nor releases old runtime image
consumers. Desired CR generation retirement and old Apply/controller exclusion are
not solved by terminal assembly observation. Saved resume/fork remains separate.
Generic Build/Import/Runtime observations and generic binding/release methods cannot
bypass a snapshot marker. Whole-operation UNKNOWN fences both roles without TTL.

## Actual bounded verification and disclosed failures

Tests use the existing opt-in `REGISTRYPINS_TEST_POSTGRES` fresh-database fixture
with an explicitly supplied disposable loopback PG17; they do not start/pull a
container, resolve a registry, call a cloud or execute captures. A second pool
defaults to REPEATABLE READ to test RC-first behavior.

- Pure validation, canonical refs/strict hashes/IDs, proof copy-once races and
  redacted value/pointer formatting passed offline and under Go1.25 race.
- Actual new PostgreSQL matrix covers typed destination publication, exact BASE
  terminal release, all generic/cross-action bypass refusal, immutable UID/spec,
  full-row/role-epoch rollback, unused cancellation and DB-failure proof burn,
  UNKNOWN permanent hold, failed DEST retention, whole-set scope/prune exclusion,
  inherited both-DB rejection and an advisory-queued RR receipt invalidation.
- Compatible BUILD/IMPORT-copy/SNAPSHOT contenders have uncontended positive
  controls before their actual races. RUNTIME's consumer receipt set is incompatible
  with a producer's DEST tag: reciprocal sequential UUID history checks and an
  explicit global UUID lock barrier are tested instead of pretending both are
  equally eligible on the same pin set.
- The full existing BUILD/IMPORT/RUNTIME and new SNAPSHOT core tests ran against
  owned PG17 with `-race`; existing regression bodies and assertions are unchanged.
  The final full run including the explicit UUID barrier passed (10.884s).
- Initial new-test compile failed on an unused pgx import. First PG run4.244s failed
  because generic DEST binding used the wrong repository and cross-host DEST stayed
  on the original host. Second4.287s exposed a managed import-copy fixture that
  should have been managed verify for BASE or external copy for DEST. Only fixture
  arguments were corrected; production guards/error classes were not relaxed.
  Corrected new matrix4.351s and full core race10.478s/10.884s passed. These failures
  remain failures, not successful production evidence.
- A sanitized catalog test exports only constraints, columns and index definitions
  from actual Ensure; no marker rows, environment or secrets. Compile-only use of
  integration packages must be `go test -c`, not a `-run '^$'` TestMain execution.

No platform/cloud caller test, live snapshot, registry push/pull/delete, managed
resume/fork, financial action, SMTP, key/flag enablement or full-writer/GC coverage
is claimed. Parent independently reviews the exact immutable source, runs all ten
local gates, uses normal head-matched merge/tree verification and separately owns
any empty-table schema-only acceptance. The whole-repository Cloudflare audit
remains a later gate immediately before GPU-last, not this test report.
