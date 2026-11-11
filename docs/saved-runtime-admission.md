# Managed saved-runtime admission: explicit safety hold

## Decision, not a restore protocol

Managed saved RESUME, paused CONNECT/wake and fork have no reviewed protocol that
excludes an already-issued old Kubernetes Apply or controller recreation. Current
snapshot capture also lacks the complete source-incarnation/provider/publication
lineage needed for that handoff. CR NotFound, lease age, a new operation/head CAS,
old CREATE annotations, and dormant SnapshotProof correlation hashes are not those
proofs. This change therefore hardens **admission only**, not full B2 support.

`SavedLifecycleAvailable(Config)` is false. There is no public/private flag, nil
wiring exception, test-only enablement or fake proof. The deliberate availability
change covers manual/native-abuse/automatic pause, resume, paused CONNECT/wake,
fork, and new AutoPause/AutoResume promises. All normal snapshot outputs are managed
registry images even when the original source was a builtin/library image, so
allowing a new pause while refusing its restore would strand a workload.

The fixed capability error is HTTP403:

> Managed snapshot resume/fork is held at this location; retained state is unchanged.
> Contact the platform operator.

No IDs, images, digests, environment, raw saved CRs or credentials are in that
message. Wake authentication and billing checks remain before capability; the proxy
passes 403 through without converting it to retry-later.

## C4 context and components

```text
Authenticated native / E2B / wake / existing scheduled action
  |
  +-- existing auth, tenant identity, usage, context and input checks
  |
  +-- saved lifecycle capability: FALSE --> fixed403 / no lifecycle effect
  |      resume: before claim/lease/uncertainty/ArmSavedStartup
  |      fork: before List/SavedSandbox/capture/clone/source-drop
  |      pause: before capture advisory/reservation/ShutdownAt/delete
  |      unfinished driver: before renewal/read/finalization/delete/abandon
  |
  +-- distinct capture/export capability (existing stores/config)
         named snapshot --> existing assembly and storage metering
         ready named image --> ordinary pinned CREATE --> usable runtime

Database: existing paused/template/cache/billing/registry records only
Kubernetes: no saved lifecycle dispatch from held entrypoints
Registry core: unchanged; both real manifest DELETE paths remain HOLD
```

Core admission lives in `internal/cloud/sandbox/runtime_saved.go` and the earliest
part of `ResumeSandbox`. Platform lifecycle guards are in the existing project
handlers/drivers. No registry schema, lease/head/operation protocol, controller,
worker, infrastructure or cloud transport is introduced.

## Strict low-level compatibility is not platform recovery

`CheckSavedRuntimeImage` accepts only an **exact image string currently present in
trusted `Config.Templates`**, with a positive canonical external or same-host
`library/` representation. It requires a valid current canonical
`TemplateRegistry=host/t`, even for an external image. This is a deliberate
conservative compatibility boundary: no-registry/short-name saved starts are held.
The platform saved lifecycle remains false even if this low-level check succeeds.

Managed `t/<project>/<template>` images are always held, including when an operator
catalog entry happens to name one. Foreign, malformed, unknown same-host namespaces,
URLs/userinfo/query/fragment, host aliases, combined tag@digest, missing policy and
unconfigured arbitrary external/library references are denied. Copied curated,
template or CREATE annotations do not authorize the actual target image.

Low-level ordering is context cancellation, saved namespace/project/ID scope,
existing armed or explicit deadline validation, then actual-target admission. All
occur before changing the saved map, egress annotations, namespace, limits, GPU,
Apply or egress network policy. The real and `NewWithAPI` compositions use exactly
the same guard; tests do not restore the old algorithm behind a switch.

## Preserved state and explicit discard

High-level restore validation precedes any claimResume, stale-pause reset/cancel,
ResumeUncertain marker, map arming, token change, archive or billing/cache insertion.
Pause/fork first use a read-only scoped cache/paused identity to preserve an own
resource404 boundary; a foreign ID does not become a capability oracle. Invalid
memory/count/TTL/configuration checks keep their existing precedence.

Existing finish/redrive/stop/renew/abandon/cancel pause drivers stop before lifecycle
writes or cleanup. They do not flow a capability error into abandonment, delete a
copy, reset a lease, or rearm the source TTL. No new background wait loop is started.
An expired INTERNAL fork snapshot is retained before retirement/log cleanup or its
automatic ForkUntil deletion; its original expiry is not backdated/reset.

This is **not indefinite live source preservation**. The existing shutdownTime can
expire independently. Retention promises that refusal does not discard the stored
state, not that the source runs forever or the copy is already safely restorable.

Generic named/capture build sync remains active and may legitimately finalize
existing captures or update diagnostic archive bookkeeping. It is not universally
byte-noop. Lifecycle-refusal tests isolate already archived/cleaned historical READY
copies; expired INTERNAL fork retention deliberately tests dirty/retired metadata.

Explicit native paused DELETE, E2B kill and project teardown retain their existing
authorization and ResumeUncertain/active stopping-resuming lease fences. No pin is
released by a new capability rule. An operator must not infer recovery permission
from the list, from an old source being absent, or from a physical source deletion.

## Truthful capability surface

The authenticated template capabilities response adds:

- `snapshotCaptureEnabled`: existing capture/export store/config availability;
- `savedLifecycleEnabled:false`;
- `savedLifecycleReason`: the fixed nonsecret held reason.

The safe native paused list adds `resumeAvailable:false` and
`resumeUnavailableReason`. It still never exposes Saved/env/access tokens or image
references. MCP create descriptions and provider/operator/client guidance state the
same restriction; ordinary no-AutoPause CREATE and live CONNECT/GET/exec/timeout
remain supported. Named snapshot capture/retake and ready-snapshot pinned CREATE
remain executed positive controls, not silently disabled along with recovery.

## Test contract transition and actual evidence

Earlier tests for successful managed pause/resume/fork are intentionally replaced
with honestly named current-admission/state-preservation tests. They are **not an
unchanged managed restore-success regression suite**, and no historical bodies are
skipped or run with a test-only capability exception. The approved replacements
retain memory/auth/foreign project/wake-token/suspension/input precedence, native
list privacy, no source deletion/resurrection, deadlines, billing and explicit
uncertainty/lease-fenced discard.

Executed owned-loopback PG17 matrix covers historical paused, pausing, stopping,
resuming, expired-lease and ResumeUncertain states with full row hashes; native/E2B
resume, paused CONNECT, authenticated wake, pause/fork and scheduler refusals; dirty
expired internal-fork retention; native-abuse detection/audit before refused action;
explicit kill/discard/teardown fences and live access positives. No actual Kubernetes,
registry, SMTP, financial gateway or lab workload is contacted by those tests.

Positive named capture tests execute truncated gzip validation, failed assembly,
source retention/no failed-output billing, named list/tenant isolation, late503 to
ready201 pinned CREATE and the byte-unchanged retake/deduplicated billed-size test.
The real GB-hour charge assertions remain 2GB×3h×0.05=0.30 then0.40 after one more
hour. Held resume403 preserves the complete storage row; explicit authorized discard
archives it and the next charge remains0.40. No monetary assertion was dropped.

Initial focused fixture tests failed because the fake's CR map also contains Cilium
policies and a release-gate fixture was missing; corrected fixture counting/checker
then focused tests passed. First PG matrix **FAILED9.055s**, not waived: an explicit
Sync on a newly seeded historical READY build legitimately added terminal diagnostic
bookkeeping. Sanitized diagnostic0.220s identified only `buildResources` default
reserialization and `diagnostics.{lastCaptureAt,logsUnavailable,revision,
terminalAttempts,terminalCaptureAt,terminalSignature,warnings}`. AutoPause itself
changed nothing. The historical lifecycle fixture was narrowly seeded as genuinely
archived+cleaned before its baseline; full equality stayed intact, no fields were
ignored and production `buildNeedsSync` was unchanged. The dirty/retired expired-fork
fixture still proves cleanup is skipped before any bookkeeping. Corrected whole
matrix PASS8.913s; full sandbox/project race PASS1.985/6.194s and owned PG matrix plus
unchanged live-access regressions race PASS14.609s. Execution is bounded evidence,
not full feature delivery or a live restored sandbox claim.

Temporary TestMain-only overlay points the existing integration suite at an owned
cached PostgreSQL17 bound to literal127.0.0.1, read-only root, bounded tmpfs and no
mounts. Per-test fresh DB body remains unchanged. Compile-only uses `go test -c`, not
`-run^$` that would execute TestMain. Raw failures/overlay/provenance are retained
outside committed source; exact owned container identity is verified before removal.

## Completion boundary

This slice does not implement source/provider/publication lineage, enforceable old
Apply/controller exclusion, new consumer ownership/head handoff, legacy adoption,
SnapshotProof caller integration, desired-generation retirement or safe registry GC.
Those require separate reviewed protocols. No public replay/clear-hold endpoint or
feature enable flag is added. A no-workload API/capability upgrade is separate from
local proofs and does not authorize a real pause/restore/fork or key/config change.
