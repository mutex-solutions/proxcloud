# C4 model change log

Every architecture change lands here in the same PR that changes the code and the
diagrams: what changed, why, and which diagrams were updated. Newest first. Format
and rules: [README.md](README.md#maintenance-rule).

## 2026-11-14: VM export tool image built by CI

**What changed:** a new workflow, `vm-export-tools.yml`, builds the image the VM export
containers run from (`images/vm_export/tools/Containerfile`: `qemu-img`, Python with
libguestfs, the appliance kernel) on a hosted runner, smoke-tests it without network as an
arbitrary uid, scans it with the existing Trivy gate and pushes it to
`ghcr.io/mutex-solutions/proxcloud-vm-export-tools` under one immutable tag. The export
pipeline is unchanged: `capsule.py` still pins the image ID it starts containers from.

**Why:** the tool image was built on a workstation and loaded onto the runner by hand.
Every public artifact, and every tool that produces one, has to come from the repository
through GitHub Actions. A reviewed follow-up change pins the published image in
`capsule.py`; until then the hand-loaded image stays, as a transitional state.

**Views updated:** [C2c build and delivery](c2-containers.md#c2c-build-and-delivery).

## 2026-11-14: One-host Docker Compose install of the portal

**What changed:** a third delivery path next to the Helm chart and the Ansible installer:
`deploy/compose` with `proxcloud-up.sh` runs Traefik, Keycloak, PostgreSQL, the API and
both consoles from the released images on one Docker host. It installs the portal only.
The Keycloak realm files are generated from the chart's templates; brute-force detection
is now on in those templates, so chart installs get it at their next upgrade.

**Why:** a first install needed either a Kubernetes cluster or the full installer. A
single host with one command is the entry path for evaluation and small sites; managed
services are added later from the portal, not by this path.

**Views updated:** [C2c build and delivery](c2-containers.md#c2c-build-and-delivery).

## 2026-11-11: Managed PostgreSQL patch and fresh catalog baseline

**What changed:** database chart 0.5.1 maps PG majors 15/16/17/18 to patch images
15.19/16.15/17.11/18.6. A newly registered DBaaS provider offers 17 and 18, defaulting
to 18. Installer name-absence/POST-only behavior and existing database chart pins
remain unchanged. Native chart renders and data-only catalog checks are required.

**Why:** security patch freshness and a PG18 offer are separate from existing
PG17 data migration. Updating a source map must not masquerade as safe major
upgrade, operator compatibility, backup/restore or deployed digest evidence.

**Views updated:** [version/catalog boundary view](../../managed-postgresql-version-baseline.md),
chart index and C4 index. No data operation, provider re-pin or publication occurs.

## 2026-11-11: Managed Kubernetes Gateway API addon

**What changed:** tenant chart 0.1.10 adds NGINX Gateway Fabric 2.8.1 and vendored
Gateway API Standard 1.6.3 carrier Secrets/ClusterResourceSet. The API/UI offer a
new explicit opt-in, not an alias for ingress-nginx. The actual cluster chart and
Kubernetes version are checked before effects. Sparse edits preserve newer picks
and raw legacy ingress values; platform bumps preserve legacy release identity.

**Why:** new clusters should use maintained Gateway API routing, while an unrelated
edit must not prune an old traffic-bearing release or silently convert its routes.
Standard CRDs are tenant prerequisites, not management-cluster CRD grants. CAAPH/CRS
converge independently; Helm wait is not cross-controller dependency ordering.

**Views updated:** [dedicated C1–C4 Gateway API view](../../managed-k8s-gateway-api.md),
managed Kubernetes runbook and C4 index. No new PVE credential, shared-cluster action,
automatic Gateway/TLS/DNS object or existing-cluster migration is introduced.

## 2026-11-11: Durable organization SSO alias retirement

**What changed:** an additive primary-only id-keyed retirement table records the
stored provider alias before Keycloak deletion. The rule snapshot checks exact
retirement before its zero-provider shortcut and excludes retired providers from
discovery/enforcement/reconciliation; fresh JIT checks the primary fence too.
Deletion retries preserve retirement through external and database cleanup errors.

**Why:** removing a broker does not revoke its already issued tokens, and an empty
provider set cannot turn a retired identity into a legitimate local/social broker.
Unknown realm-level brokers remain compatible; no token-controlled alias prefix
is treated as revocation authority. No guessed session-lifetime TTL is used.

**Views updated:** [dedicated C1–C4 lifecycle view](../../sso-alias-retirement.md),
SSO operator guidance and C4 index. All API readers must upgrade; prior deleted
aliases are not backfilled and warm-cache behavior during primary outage is not
promoted to a hard revocation SLA. No real realm, token or identity mutation.

## 2026-11-10: Tenant read, role-assignment and private-traffic boundaries

**What changed:** order reads now resolve the stored billing profile's owning
organization and require current billing-read permission before a typed response.
Organization invitations require role management for every custom role, retaining
ordinary MEMBER invitations and rejecting unknown role names. Sandbox pod lookup
now identifies actual envd serving mode from the main command and arguments, so
private native port 49983 cannot receive envd's traffic-token exemption.

**Why:** identifiers and port numbers cannot replace authority. A custom role's
permissions can change after it is assigned. Existing envd pods need compatible
classification without a new annotation, credential or workload recreation.

**Views updated:** [dedicated C1–C4 boundary view](../../tenant-authorization-boundaries.md),
this index, and the managed-sandbox/E2B traffic descriptions. No new container,
schema or external service; all held mail/lifecycle/registry features remain held.

## 2026-11-10: Legacy SMTP credential TLS enforcement

**What changed:** legacy SMTP credential AUTH now requires an actual completed,
verified TLS connection and certificate validity for the configured host before
AUTH capability selection. LOGIN independently checks TLS and the expected host,
and releases challenge responses only after an accepted Start. NoAuth retains
its existing unauthenticated-relay behavior; PLAIN preference is unchanged.

**Why:** a stripped STARTTLS advertisement must not select unguarded plaintext
LOGIN or silently proceed to an unauthenticated message envelope. Certificate
identity, rather than SNI text, preserves valid DNS and IP-SAN TLS connections.
No new transport, outbox enablement, schema, dependency or live effect is added.

**Views updated:** [dedicated C1–C4 safety view](../../smtp-credential-tls-safety.md)
and the C4 index. Production changes are confined to the existing SMTP component;
new bounded fake-peer tests do not establish live relay or release acceptance.

## 2026-11-10: Documentation reconciliation against current code (PR #176)

**What changed:** corrected current C1/C2 labels for implemented template pipelines,
split installer stages and configured shared backup integration; qualified GHCR
publication, runtime acceptance, held sandbox saved lifecycle and registry deletion.
The C4 legend now distinguishes existing code from enabled/released/accepted
behavior. Added current support/release and CI operation guides and refreshed
installer/product/developer documentation. Original ADR decisions and old C4
entries remain historical with explicit supersession notes where needed.

**Why:** old labels and runbooks still described mandatory RabbitMQ, absent
pipelines, automatic pruning or publication, and all-feature readiness that the
current code/configuration does not promise. Documentation changes do not create
a release, run a cluster operation or certify untested paths.

**Views updated:** C1 external systems, C2 current container/integration status and
C4 README semantics. No runtime architecture or capability was changed.

## 2026-11-10: Kubernetes template static build seed delivery (PR #175)

**What changed:** the existing node-template workflow gains an explicit static
network mode. A private local NoCloud `cidata` ISO supplies the live Ubuntu
installer and installed build VM network, with an explicit Packer SSH host rather
than DHCP address discovery or an inbound HTTP seed. Temporary build identity,
static configuration and seed media are sanitized before node-template tagging.
Existing DHCP mode is preserved as an explicit alternative.

**Why:** an external DHCP server is not a Kubernetes requirement, and an HTTP
seed cannot supply initial connectivity when the guest has no lease. The seed
contains temporary credentials/topology; exact object ownership, private file
permissions and verified detachment/deletion are part of the delivery boundary.
A cancelled job or failed build does not prove cleanup; no broad storage deletion
or host DHCP changes are implied.

**Views updated:** C2c build/delivery diagram, container table and trust-boundary
notes in [C2](c2-containers.md#c2c-build-and-delivery). Synthetic tests and Packer
configuration validation are not live installer/reboot/tenant-node acceptance.

## 2026-11-10: DBaaS read endpoint / replica invariant

**What changed:** replica scaling revalidates the current Application callback's
engine, offered count and explicit reader policy before mutation. An enabled
reader downscale below two returns 409 with an explicit-disable-first instruction.
The shared CREATE/SET rule retains existing 400 messages; sync projects reader
flags/host only for supported engines, exact true and a valid declared count >=2.

**Why:** a legacy one-instance reader can have no PostgreSQL RO Service or MariaDB
reader backend yet remain advertised and marked as a chargeable PostgreSQL reader
LB. Exclude that structurally impossible projection without rewriting desired
policy, chart pin or historical money. Configured feature flags are not physical
LB/readiness proof; full-spec SSA is not backend atomicity or lost-update fencing.

**Views updated:** [bounded C4/safety guide](../../dbaas-read-endpoint-invariant.md)
(component/validation/sync sequence and acceptance limits), plus C4 index. Local
fake-action/race and cached Helm rendering are not live DB engine acceptance.

## 2026-11-10: PVE single-backup deletion known-active interlock

**What changed:** the existing single `DELETE_BACKUP` path now reads current VM
config and the token-visible active restore list after its original VM/archive
ownership checks, refusing any config lock, active restore or read failure before
content DELETE. Missing/null config is refused locally; `RestoreRunning` now treats
missing/null task data as unavailable, while an explicit empty list remains idle.

**Why:** bulk deletion already refused known backup/restore activity, but the single
archive path could delete a restore's input. This is not an atomic task fence:
post-check task starts, VMID replacement, external/root writers and timeout/crash
uncertainty remain outside scope. No new lock, wrapper, schema or operation engine.
Backup/restore/retire ordering and existing task query/authorization stay intact.

**Views updated:** [bounded C4/safety guide](../../pve-backup-delete-interlock.md)
(request sequence, token visibility, shared-helper hardening and explicit limits),
plus C4 index. Hermetic wire/unit/race evidence is not real PVE/PBS acceptance.

## 2026-11-10: managed saved-runtime admission-only safety boundary

**What changed:** a static-false saved-lifecycle capability and strict low-level
current-curated-image compatibility check now refuse managed resume, paused CONNECT/
wake, fork, new pause and AutoPause/AutoResume promises before lifecycle effects.
Unfinished pause drivers and expired internal-fork automatic cleanup retain state;
capabilities/list metadata, MCP and operator/client guidance report the hold.

**Why:** old issued Apply/controller recreation and complete snapshot source/provider/
publication lineage are not excluded by CR NotFound, new head CAS, copied CREATE
annotations or dormant proof correlations. This deliberately reduces availability;
it is not full B2. No flag/test exception, lease/head protocol, schema or GC is added.
Capture/export and pinned ordinary CREATE, live access and fenced explicit discard
remain separate. Source TTL can independently expire and is never secretly rearmed.

**Views updated:** [dedicated C4/safety/compatibility guide](../../saved-runtime-admission.md),
C4 index, managed-sandbox/E2B references and client help. Tests intentionally replace
historical managed-success cases with actual admission/retention evidence, not an
unchanged restore-success claim. Local owned PG/race is not live restore acceptance.

## 2026-11-10: dormant typed snapshot registry dispatch prerequisite

**What changed:** a separate fixed `snapshot/v1` proof and empty dispatch table,
credential-free frozen source/provider/action correlations, exact full pending pin
set and role epochs, and reciprocal BUILD/IMPORT/RUNTIME/SNAPSHOT UUID exclusion.
Typed assembly observation permits managed BASE terminal release and successful
DEST publication handoff only; failed/published DESTs have no retirement shortcut.

**Why:** snapshot append is not content-preserving import. Reusing producer or
runtime receipts would skip action/source/publication obligations. Source stamps
are correlation, not current capture authority; no caller, feature flag, cluster
or registry I/O, legacy adoption, real DELETE or full-writer coverage is enabled.

**Views updated:** [dedicated C4/safety guide](../../registry-snapshot-core.md)
(context, owned PRIMARY/RC sequence, source trust boundary, role lifecycle and
conservative retention), plus C4 index. Owned loopback PostgreSQL and race proofs
are bounded core evidence, not live snapshot/resume/fork or GC acceptance.

## 2026-11-10: PR #170 typed savings contract-expired outbox (default OFF)

**What changed:** fixed fourth `savings-contract-expired/v1` family, exact validated
family_v3 CHECK, and private immutable canonical-term history. Owned PRIMARY READ
COMMITTED expiration commits status, reminder cancellation and frozen mail selection
together; all four contract mutation paths cancel pre-intent IDs even OFF/keyless.

**Why:** expiration notices must survive the decision boundary without duplicate
same-EndDate reactivation/reapproval or historical adoption. One notice per canonical
term is deliberately conservative; no lifecycle UUID, replay endpoint, financial
posting or SMTP dispatcher activation is introduced.

**Views updated:** [dedicated C4/safety/default-OFF view](../../savings-expired-outbox.md)
(context, components, transaction sequence, history and cancellation lifecycle),
and [C4 index](README.md). Original three-family payloads remain unchanged; local
bounded unit/race and owned PG17 proofs do not authorize deployment or delivery.

## 2026-11-10: typed validation approval outbox (enqueue-only)

**What changed:** new default-OFF `validation-approved/v1` family and fixed
`billing_profile_validation_approved` system template attest only the approval of
`billingProfileValidation`, not account activation. An owned PRIMARY RC transaction
locks validation then current profile, snapshots one fixed template/config and
recipients, and atomically commits status, fresh generation/history and sealed
mail. Same-generation repeats and historical APPROVED rows never re-render or
backfill. Existing pending/rejected decisions cancel unsent IDs even OFF/keyless;
sticky history prevents legacy fallback. The exact v1-to-v2 CHECK migration admits
only three version-1 families and leaves prior ciphertext/AAD/wire unchanged.

**Why:** status commit must not rely on later best-effort mail, and the old
fully-activated wording is untrue before external activation completes. Activate
stays postcommit; financial/provisioning/cloud effects are outside mail atomicity.
No production dispatcher, activation retry, delivery or inbox guarantee is added.
All readers/writers must upgrade before opt-in; older-family binary rollback onto
the v2 schema is unsupported. Producer OFF is not a schema downgrade.

**Views updated:** [dedicated C4 view](../../c4-validation-approved-outbox.md),
[safety/default-OFF guide](../../validation-approved-outbox.md), C4 index and docs
index. Local offline Go1.25 unit/race and owned PG17 proofs are bounded, not a
whole-suite, deployment, real activation or external SMTP claim.

## 2026-11-10: typed PROJECT_INVITE transactional outbox (enqueue-only)

**What changed:** default-OFF fixed `project-invite/v1` single-recipient family;
primary invite insert, one template snapshot, sealed enqueue and durable IDs now
share one owned transaction. All-match accept/decline/expiry deletion atomically
cancels persisted pre-intent rows even with producer OFF or keys absent. Schema
Ensure transactionally upgrades the original single-savings CHECK to exactly two
version-1 families, preserving existing Savings ciphertext/AAD and immutable triggers.

**Why:** a committed invitation token must not depend on a later best-effort mail
call. Rendered invitation URLs introduce a narrowly reviewed credential-bearing
AEAD body boundary, not arbitrary event/template/URL jobs. Dispatch remains OFF:
queueing is not sending, acceptance is not inbox delivery, and already-DISPATCHING
or UNKNOWN work cannot be unsent/replayed by business deletion or expiry.

**Views updated:** new project-invite-outbox.md (C3/C4 components, transaction,
credential/storage boundaries, cancellation lifecycle and availability limits),
README.md navigation and docs/mail-outbox-safety-limits.md compatibility contract.
All readers/writers must upgrade before separately authorized opt-in. Local Go1.25
race and owned PG17 evidence is finite; no deployment/SMTP activation/full-suite claim.

## 2026-11-10: fail fast on authoritative sandbox startup lifetime expiry

**What changed:** the existing Ready-clock lifetime guard distinguishes a positively
expired current provisional end or persisted UID-scoped Ready-candidate runtime end
from transient control-plane deadlines. `WaitReady` immediately returns a terminal
runtime startup error with a fixed public reason; the private expiry marker still
unwraps to `context.DeadlineExceeded` for existing callers.

**Why:** a Ready CR whose authoritative lifetime has already ended cannot become
usable. Treating its expiry as an unavailable control plane needlessly retried until
the default 120-second scheduling budget. Transport deadlines remain retryable within
the original caller/phase/total budgets; malformed/future timestamps and UID/RV CAS
checks retain their existing behavior. No TTL renewal or pending billing is granted.

**Views updated:** the startup exception in sandbox-runtime-parity.md. Local finite
Go1.25 unit/race regressions cover one-read/no-patch expiry, unchanged first-candidate
TTL, no Ready-clock pending billing, and recovery after transient network errors.
No scheduler wall-time, deployment, registry release or live-runtime claim is made.

## 2026-11-10: dormant typed runtime reference fence (bounded P2-B engine)

**What changed:** distinct RuntimeSpec/RuntimeProof/RuntimePodEvidence, one immutable
managed consumer pin, durable PRIMARY one-shot runtime marker, shared-copy local
consume/cancel and global Build/Import/Runtime UUID exclusion. Private typed guards
compose after both producer guards; only exact independently recorded pod/CR/spec
provenance binds a running consumer or releases its known terminal incarnation.
Full marker/receipt epoch CAS, monotone terminal state and nonexpiring UNKNOWN
retain uncertainty without replay, TTL cleanup or a generic release grant.

**Why:** a runtime is a long-lived image consumer, not a completed image producer.
Startup Ready must retain the image; copied receipts and CR UID reuse on resume
cannot grant dispatch or release. Desired owner retirement and exclusion of old
Apply/controller recreation remain trusted future-caller prerequisites, not a
claim that pod phase alone proves whole-product release safety.

**Views updated:** dedicated runtime reference context/container/lifecycle contract
and remaining-writer ledger in runtime-reference-fence.md. This engine is dormant:
no native/E2B caller wiring or all-writer/adoption claim, and BOTH real-delete HOLD
paths remain unchanged. Focused Go1.25 unit/race plus opt-in disposable loopback
PostgreSQL tests cover strict specs, lifecycle, kind/epoch fencing and lock ordering.

## 2026-11-09: sandbox runtime parity core from pinned EE source

**What changed:** phased bounded startup and a finite Ready-clock runtime TTL,
UID/RV durable Ready candidate/final lifetime commit, authoritative environment/ID
budgets and PID1 mode guards; independent builder resources and stable diagnostics;
operator-scoped no-build digest import worker and optional finite pull-only warming.

**Why:** slow image pulls must not consume runtime TTL or fabricate billing/usable
Ready, retry/flap must not refresh a durable lifetime origin, and source credentials
must not cross exact repository/project boundaries. Warming is current-policy,
context-bounded and runtime-capability gated, never customer-code execution/cache GC.

**Views updated:** sandbox core code invariant table and runtime adaptation contract.
Focused Go1.25 unit/race and loopback OCI tests cover the new contracts; preserve
ProxCloud's native/egress/readiness/abuse, force-OCI directory handoff, credential-free
prepare-output and durable BUILD dispatch guards. No live publication, rollout,
RBAC/labels, arbitrary registry source fallback or real-delete activation is claimed.

## 2026-11-09: registry BUILD/template participation (P2-A)

**What changed:** startup schema + BUILD-only wiring; stable owner/nonce PREPARING,
BASE+DEST pins committed before use/project TX, locked candidate revalidation and
one durable dispatch marker. Production StartBuild requires the unique opaque
proof, copied proofs share use/cancel state, and positively unused cancellation
keeps its no-remint marker. Exact pod annotations/UID evidence retain BASE aliases
until terminal; durable terminal owner checkpoints resume per-pin handoff after
pod removal. READY DEST bindings survive retained historical builds/tag edits;
trim/delete tombstones retain exact cleanup generations. Compact retired build-ID
identities survive full receipt cleanup and refuse stale pre-prepare resurrection.
All public readiness projections/usable selections wait for completed handoff;
current retained From/Image refs conservatively HOLD parent cleanup, without
claiming complete durable provenance pin coverage. Independent retired generations
continue cleanup before aggregated HOLDs return, so an eligible retired child
cannot starve its held parent forever; same-digest ambiguity remains conservative.

**Why:** retrying an ambiguous apply or independently minting in-memory proofs
could dispatch twice; stale saves/error handlers could lose completed tags or
poison a known owner. Full owner CAS and resumable exact terminal transitions keep
uncertainty visible without a lease, guessed success or SQL transaction around
new network effects. Other writers/adoption are not covered; BOTH real-delete
paths stay unconditional HOLD.

**Diagrams updated:** BUILD participation code invariant table and tracked operator
contract. Focused unit/race + real-PG/fake-K8s/loopback-S3 cover admission/one apply,
known refusal vs UNKNOWN, proof copy/cancel/no remint, generation cleanup and owned
BASE+DEST crash checkpoints before/after each handoff with no old pod. No live
registry/Kubernetes actions or real-GC acceptance are claimed.

## 2026-11-09: dormant durable registry pins and real-delete HOLD (PR1)

**What changed:** add an explicit PRIMARY DB-only pin/prune-intent core, canonical
trusted-host/current-project scope checks, independent-commit inherited-TX refusal,
READ COMMITTED namespace-first admission, full owner/generation/epoch receipts,
observed binding generation cleanup and nonexpiring UNKNOWN/SENDING holds. Both
scheduled/debug and project-teardown real manifest deletion return typed operator
HOLD; off/dry-run remain unchanged. No startup schema call, writer integration,
worker, lease, coverage flag, HTTP effect or new dependency is introduced.

**Why:** finite scans cannot fence a new reference after their last observation;
lease/epoch changes cannot stop an old registry HTTP DELETE. Missing receipts must
not auto-expire, resend or silently report completed project cleanup. Preserve
requested alias identities so different aliases may bind the same digest safely.

**Diagrams updated:** C4 code component/invariant table plus the bounded operator
contract. Focused race/unit and six local real-PG tests cover both admission orders,
RR snapshot defaults, same-UID stale-generation cleanup, alias convergence,
UNKNOWN age/no-replay, project scope/inherited transactions and ON/HTTP0 holds.
Writer participation + full legacy adoption (PR2), then separately approved exact
scoped GC (PR3), remain prerequisites; no real-delete acceptance is claimed.

## 2026-11-09: bounded volume headroom and snapshot retype guards

**What changed:** EXTEND takes required provider headroom and verifies current
nonpending live disk size and same-token node/storage capacity before its existing
R1 intent. Reserve the exact growth bytes, not the full target. Missing/null,
negative, over-total or overflowing metrics and unknown disk sizes refuse409.
SERVER RESIZE root growth joins that guarded path before shutdown/flavor writes;
growth/backend failures are no longer swallowed. RETYPE checks an authoritative
snapshot list before `MarkMoving` and repeats inside the service holder session:
ignore `current`, conservatively refuse any real VM snapshot or unknown inventory.

**Why:** cache quota size is not capacity evidence, thin/shared storage still
needs the configured safety margin, and a PVE snapshot prevents delete=1 moves.
A predictable refusal must not leave a moving mark, stop a VM or create a mutation
intent. No automatic snapshot deletion, root credential fallback, tenant headroom
override, lease-engine changes or claim of atomicity against external operators.

**Diagrams updated:** C4 guarded volume sequence/invariant table and operator UX
repair guidance. Existing create/rebuild/full-copy placement and heavy move
budgets are unchanged. Focused local fake-wire and real PostgreSQL row-follow
regressions; deployment and owned-fixture acceptance are separate normal gates.

## 2026-11-09: atomic savings reminder decisions (enqueue-only PR B)

**What changed:** an explicit default-false reminder producer flag couples the
current ACTIVE/expiry-generation/tightest-window decision, one rendered template
snapshot, strict encrypted outbox enqueue and queued/skipped bookkeeping in one
PRIMARY transaction. Schedule identity is contract + canonical expiry, not random
reminder ID; recipient metadata uses per-event ordinals, not email hashes. Local
client/admin cancel/extend/expiry/delete joins contract -> reminder -> outbox locks
and cancels only pre-intent work. A keyless cancellation capability stays wired
when the producer is OFF; any durable history prevents legacy SMTP fallback.
Admin no-op/render-only edits preserve future windows and immutable selected mail.
Batch candidates are identity hints only: the candidate row is reread under the
contract/reminder locks before legacy adoption, so an expiry mutation cannot turn
a stale snapshot into an empty DONE schedule that poisons the next generation.
Creation rejects empty window sets; a deterministic PG regression proves both
client/admin expiry changes still permit the new generation's later due enqueue.

**Why:** SMTP-before-SentAt and save-before-notify had crash gaps; ignored errors
could mark missing messages as sent. A producer rollback, existing admin raw edit
or competing scheduler must not reopen or strand an old notification generation.
Queued/suppressed/blocked/no-recipient are not delivery evidence; legacy processed
SentAt remains no-resend evidence, with conservative migration for ambiguous history.

**Diagrams updated:** C4 decision+enqueue sequence and contract invariants; safety
limits document the producer flag, mandatory key, migration/OFF behavior and typed
counts. SMTP dispatcher remains OFF, other notification families best-effort,
no resend/backfill/real-recipient rollout or timed billing changes. Targeted real
PostgreSQL tests cover two schedulers/processors, atomic rollback, keyless OFF
cancellation, admin edits/no-op/delete, legacy evidence and canceled claim fences.

## 2026-11-09: dormant durable reminder mail infrastructure (PR A)

**What changed:** dedicated primary `mail_outbox`/event fingerprint/append-only attempt tables;
owned-business-transaction enqueue of one frozen recipient envelope per row, additive strict
key-ID AEAD blobs, typed bounded SMTP and opaque RBAC read-only status. T2 intent commits before
connect, no SQL transaction surrounds SMTP, expired intents remain UNKNOWN, and late definitive
receipts bind only to the same immutable attempt. Dispatch is OFF: no worker registration,
business producer, cron, backfill, production enable flag or resend API. Legacy mail/textcrypt
behavior, broker/Compose/chart configuration and all notification callers are unchanged.

**Why:** a database-only durable-job completion cannot atomically ACK SMTP, and a lease cannot
kill an old SMTP socket. Lost EOD receipts must not silently resend; random ciphertext cannot
serve immutable deduplication. Per-recipient sealed snapshots, keyed plaintext fingerprints and
explicit UNKNOWN holds make the uncertainty and privacy boundaries reviewable before PR B's
atomic savings-reminder decision is added.

**Diagrams updated:** C4 mail-intent sequence/invariant table. [Safety and limits](../../mail-outbox-safety-limits.md)
document the dormant scope, 20-recipient/1-MiB/8-attempt/30-day bounds, mandatory TLS/key behavior
and local PostgreSQL + loopback transcript evidence. No actual SMTP recipients or lab changes.

## 2026-11-09: broker-free deployment of PostgreSQL durable jobs

**What changed:** fresh Compose, Helm quickstart/defaults and installer stage 40 need no RabbitMQ
container, CR, operator, API credential or readiness dependency. The API always runs bounded
primary-DB workers; scheduler/debug default false controls new timed work/routes, not an existing
queue pause. Concurrency/poll/lease are wired explicitly. Optional reader DSNs live in Secrets;
CNPG `-ro` is generated only with explicit status-reader opt-in and replicas, while queue/finance
remain on writable `-rw`. Old API Secrets may omit the optional reader key. External billing HTTP
behavior is unchanged; this first slice is engine/native billing, **not durable mail**.

**Why:** PostgreSQL is already authoritative for billing; durable, fenced work should not depend
on another broker/operator or disappear with an API process. Removing desired resources must not
remove historical data: existing upgrades require explicit stopped-producer/consumer and
queue-drain/reconciliation/snapshot evidence. Helm removal needs a prior stored keep-manifest
transition, Argo pruning needs live Prune/Delete=false protection before applying new state;
stage 40 only reads and asserts, never patches/deletes a broker/PVC/operator. Compose preserves
the old service/image and anonymous volume identity behind an explicit legacy profile; operators
replace only the API, not down/remove/recreate data. A legacy profile/timeUnit message has no
original cutoff, so automatic current-cycle replay is forbidden by the operator runbook.

**Diagrams updated:** README overview, architecture boot/container views, C2 management workers/reader,
C3 billing/platform and C4 rating path. [Cutover runbook](../../postgres-jobs-migration.md).
Validation is local rendering/lint and mocked installer migration gates; no existing lab resource
or deployment is changed by this PR.

## 2026-11-09: primary durable native billing and transactional enqueue

**What changed:** the API no longer connects to RabbitMQ. Native dispatch enqueues a versioned original profile/cadence/cutoff on PRIMARY; bounded competing workers use SKIP LOCKED, DB-clock UUID-owner/epoch leases and fenced retry/ACK. The DB-only worker joins all service bill mutations, a retained financial receipt and ACK in one pgdoc transaction; `EnqueueTx` joins the same business commit and rejects wrong-database contexts at every Store boundary. Monthly clock and deletion accrual honor original cutoffs; watermarks never rewind. Issued cycles require reconciliation, and invoice issuance defers on known unresolved backlog. Status is bounded/lag-tolerant with an optional separate read-only pool; authorization/replay/claims/finance never use it. Replay preserves immutable payload and commits operator audit. Legacy fanout true is a migration error, not a silently dropped queue.

**Why:** a delayed Rabbit payload had no original cutoff, poison was dropped and business commit versus broker ACK was not atomic. A read replica cannot coordinate leases/receipts or fresh security decisions. Stock compatible River completion/rescue paths did not fence a superseded attempt; a compact fixed-kind primary module avoids a generic framework/fork. At-least-once execution plus financial receipt is not external exactly-once behavior.

**Diagrams updated:** C4 durable job sequence and invariant table. Deployment C2/C3 removal/configuration is a separate PR after legacy drain/reconciliation; mail/invite outbox and cross-pod SSE are explicitly not claimed. Real PostgreSQL tests cover ownership/crash/transaction boundaries and actual bill money/units; operator migration/retention evidence is in `docs/durable-jobs.md`.

## 2026-11-09: poll the actual worker of a cross-node seed upload

**What changed:** ISO uploads require a nonempty, structurally valid `imgcopy` UPID, but no longer require its
worker node to equal the requested storage node. The authenticated UPID selects the node polled by `WaitTask`;
configuration and content-delete endpoints retain their target-node checks. Null/missing/malformed task identities
remain unknown mutations behind the existing durable fence.

**Why:** PVE's upload handler runs on the API-entry node and copies to a different target via scp. The live seed
migration/rebuild drill targeted pve2 through pve1 and received a completed pve1 imgcopy task; treating that valid
identity as unknown unnecessarily stopped rebuild after replacing its boot disk. The regression asserts upload-to-
pve2, poll-running/OK-on-pve1, then config-attach-on-pve2 in order, without weakening uncertainty or cleanup guards.

**Diagrams updated:** none; the existing PVE client/seed component boundary is unchanged. No lab writes in this fix.

## 2026-11-09: usable small static IPv4 subnet pools

**What changed:** `VnetStaticPoolFor` uses a dedicated `staticHostRange`, independent of PVE DHCP sizing.
Static `/29` and `/30` subnets use usable host addresses; `/28` and larger keep the network+10 reservation.
Whole-address IPv4 arithmetic bounds the span to its subnet, with network/broadcast and gateway endpoints
removed. The allocator continues skipping an interior gateway, exclusions and allocated addresses. Unsupported
or malformed static subnets return an error, not empty range endpoints. DHCP keeps its existing `/28` minimum;
the network-create API still accepts `/29` or larger.

**Why:** a valid network-created `/29` reused the DHCP helper's minimum-size guard and returned an empty span,
so the subsequent server allocation received `Ranges:["-"]` and failed with an invalid-pool 503. Small static
subnets need usable hosts, not relaxed DHCP constraints or a new API rejection of existing valid networks.

**Diagrams updated:** none; C4.1 symbol table and address-allocation contract describe this internal fix.
PVE helper tests and fake-PVE/project allocator regressions cover `/29` exhaustion, `/30`, an upper `/25`,
gateway placement, DHCP sizing and malformed/IPv6/out-of-subnet pools. No lab resources were changed.

## 2026-11-09: ordinary server seed artifact ownership and guarded cleanup

**What changed:** every new ordinary server ISO receives a write-ahead artifact row with immutable installation identity
and a 128-bit random exact filename. Superseded/failed files survive independently of encrypted active seed rows.
All erase/empty paths share one fail-closed gate, cooperative replica lock and non-expiring durable operation claim
that survives session-lock connection loss. Product backup/restore pins all provider artifacts before dispatch until
operator archive/task reconciliation; this intentionally favors retention over unsafe archive-reference inference. Optional operator-provisioned global
GET-only credentials prove full ACL visibility (including exact NoAccess shadows), then scan every current/pending/
snapshot/template config. Same random basename on any storage vetoes alias references; identity drift and unknown async
mutations retain files. Two no-reference observations and 24-hour grace precede cleanup. Admin secret-free status and
Test connection warning expose pending risk. Legacy files are report/operator-owned, never prefix-adopted.

**Why:** local cache absence is not proof of global non-use, and PVE upload/delete do not check references. Upload before
active-row persistence and log-only replacement cleanup lost recovery authority. Preservation takes priority over eager
erase: no reader means pending warning, not blanking a live static-network CD or dropping its encrypted rebuild source.
No new portal grant, boot scrub, guest agent execution, URL/static-network change or lab mutation. Independent privileged
external writes still need coordination; this is not an atomic PVE compare-and-delete guarantee.

**Diagrams updated:** C3 IaaS seed journal/read-only reference gate components and edges. Operator details and deferred
live confidentiality acceptance: [server-seed-safety.md](../../server-seed-safety.md).
## 2026-11-09: DNS ownership before tenant zone creation

**What changed:** new project/provider/canonical-zone challenges return a256-bit TXT value once and retain only a scoped
SHA-256 hash. Pending30min and verified10min lifetimes, single-use durable consumption, project-wide10live/20hour
challenge quota and10s verification backoff bound the lifecycle without reserving domains. Native cloud and MCP create
share the mandatory gate. Strict IDNA+PSL validation and actual exact-owner TXT queries over bounded TCP to fixed public
recursors (or explicit operator-only literal-IP overrides) happen before creation; CNAME/DNAME indirection and resolver
failures refuse. Project/provider/permission/config-binding checks and live PowerDNS overlap checks remain at creation.
A global DB advisory lock serializes actual creates across provider aliases; creates have a10s total deadline and a
durable60s per-project attempt cooldown across proofs/providers, so a slow-backend retry cannot continuously monopolize
the shared namespace. Verification is per-project/nonblocking and
row-lock transitions cannot overwrite a consumed/cancelled proof. Consumption commits before upstream I/O, so uncertain
failure cannot replay. Tokens/hashes stay out of lifecycle audit; proof TXT mutations redact values. UI guides publish →
verify → create → delegate, with clear cache/expiry/retry instructions. MCP exposes the same lifecycle, never a shortcut.

**Why:** suffix/overlap checks alone allowed any tenant to squat an unowned domain and exclude its ancestors/descendants.
A pending challenge must not become a new domain-lock denial of service, and creating a zone first would let the platform
vouch for its own unproved tenant data. Fixed resolver trust avoids the host's split authoritative view and tenant SSRF;
this is conventional TXT control proof, not DNSSEC/legal ownership. Private lab overrides explicitly prove only controlled
drill data. Existing legacy zones are not removed or retroactively gated; upgrade every API replica before reopening creates.

**Diagrams updated:** C1 external resolver trust, C3 DNS ownership components and C4 sequence/invariants. Operator API,
upgrade, cache and lab-override details are in cloud-integration.md; customer/MCP docs describe the required flow. HTTP/
Postgres tests use real TCP DNS and fake PowerDNS, with no public DNS grant or claim of a production-delegation drill.

## 2026-11-09: functional native automatic pause and recovery

**What changed:** native pods mount the existing read-only node runtime and run a PID1-only `-native` mode that marks
boot then execs the original argv, retaining image USER/env/workdir/signals/exit behavior. Native automatic pause now
uses the working filesystem snapshot path; a project-scoped native paused list/resume/delete API recovers it without
turning envd on. Resume reuses suspension/release/quota/rotation/billing and pause claim locks. Automatic pauses persist
a credential-free policy/connection-fingerprint/sample guard and reauthorize deletion under the capture lock after
asynchronous image assembly/redrive. A changed policy, UID, activity or stale telemetry retains the original; an
unknown/absent/replaced source never causes the only usable snapshot to be discarded. Guards bind the host-owned main
container ID/restart count as well as CR/pod UID: a same-pod restart can lose the writable filesystem. Cancellation,
abandonment and stale-pause/resume adoption require positive original-filesystem identity; replacement conflicts return
409 with the copy retained for recovery after operator removal of the replacement. Actual pod and desired CR must both use
the native helper. Unsupported envd + pause/kill (including disabled provider edits) and pause without storage are
rejected explicitly in API/operator form; SDK remains alert-only.

**Why:** the prior native opt-in could never pause shipped pods (no mounted helper or boot marker), and only exposing
E2B resume stranded native snapshots. Proxy request counting is insufficient proof for detached/silent SDK processes,
HA gaps or direct bypass; this bounded native capability adds no credentials/RBAC or tenant process trust. Native
images require writable `/tmp`; upgrade node runtime before portal and recreate legacy native pods. Handler/Postgres
regressions hold assembly, prove source remains until image usable, recover files through native resume, and cover late
policy/activity/UID/stale metrics, failed capture, suspended/foreign access and expired source.

**Diagrams updated:** C3 managed-service runtime node and safety component contract. Live lab verification is a separate
post-merge gate, not claimed by the simulated cluster/image fixture.

## 2026-11-09: show the effective egress cap before sandbox creation

**What changed:** the sandbox create dialog's limits include the selected curated
template's egress cap, falling back to the provider's default before selection.
**Why:** the API and running rows already exposed the cap, but a customer could not
see that constraint before starting a sandbox. This is display-only; the host CNI
remains the enforcement boundary. **Diagrams updated:** none; C3 sandbox component
text records the client-facing limits contract and the measured SB10 status.

## 2026-11-09: nginx public asset modes independent of umask

**What changed:** both SPA images normalize only nginx's public document root to
0755 directories and 0644 files; the entrypoint sets its generated `config.js` to
0644 before starting nginx. The Docker regression checks both images and HTTP
responses under umask 077, with an existing 0600 config and a missing config.
**Why:** Vite preserves public-file modes from a restrictive checkout, and shell
redirection preserves an existing 0600 mode; nginx workers then return 403 for the
runtime config and brand SVGs, making the SPA fall back to development auth URLs.
No secret paths or global umask are changed. **Diagrams updated:** none; C2 build
and delivery text records the existing SPA containers' public-file contract.

## 2026-11-09: sandbox abuse controls and wait-only worker replacement (SB10)

**What changed**

- Provider and curated-template egress caps (100 Mbps default), Cilium bandwidth manager and per-pod annotation,
  plus a host-owned chained CNI TBF on real pod `eth0` before customer code starts. Cilium uses `customConf`;
  hostNetwork node-setup owns the chain and atomically installs the static guard from the runtime image.
- A hostNetwork unprivileged collector reads its own kubelet `stats/summary` using a separate `nodes/stats` GET
  identity and pinned public serving chain. Dedicated namespace ConfigMaps contain only CPU/network counters,
  pod UID and node identity. The portal reads those, never obtains `nodes/proxy`.
- Review hardening: CNI binds live pod UID/node to kubelet CNI arguments; installation and release gate require deployed
  Cilium customConf/no writer, and stage55 awaits it then rolls all old writers before installing the chain. Final action
  freshness validates collector/CPU/network timestamps independently; rotation scope fingerprints its encrypted connection.
  Capture/exec advisory locks use a separate bounded connection pool, so lock holders/waiters cannot exhaust the primary
  store pool needed for independent snapshot progress transactions (MaxConns=1 regression).
- Review safety: envd/E2B providers alert only; automatic pause/kill is refused because a silent SDK streaming exec
  has no trustworthy inactivity marker. Native exec serializes with the capture/action lock. Destructive actions recheck
  exact fresh policy/attachment/connection and node activity under that lock; handler/Postgres regressions cover stale
  disabled policy, reused pod, renewed exec/traffic, alert and opt-in kill, and unsupported pause.
- Minutely sustained-CPU/inactivity sampling persists consecutive windows in `sandbox_safety`; detections and
  action outcomes are audit events. Default alert, operator mail template and admin filter/project count; provider
  explicit pause/kill opt-in. Actions revalidate CR/pod UID and provider/project ownership and skip busy captures.
- Default-off wait-only worker rotation: explicit namespace/cluster/MD allowlist, NodeRestriction-protected worker
  labels and CAPI nodeRef UID, empty preflight, cordon/recheck, durable provider start fence and beta2 `rollout.after`.
  Creates, resumes, builds and snapshot assembly serialize with the fence; replacement must converge before reopening.
  No eviction, drain, force delete or automatic pause. Read-only admin plan and mgmt-only job triggers.
- Installer upgrade now reapplies stage 55 cluster as well as fresh installs. The worker Ready wait follows
  hostNetwork node-setup/CNI installation, preventing fresh-install deadlock when Cilium customConf disables its own chain writer. Admin settings, client effective cap,
  SDK-visible limits and operator runbook updated.

**Why**

- Cilium native-device EDT does not limit same-node traffic: capability spike measured about 130 Mbps despite a
  1 Mbps annotation. TCX bypasses host-veth classic police. Host-configured real pod netns TBF works independently
  of TCX; it also avoids a reconciliation startup gap. Node metrics are required because tenant `/proc` is untrusted.
- A heuristic can match legitimate unattended compute: default alerts preserve data. Missing/stale samples, gaps,
  activity or identity/counter changes reset evidence. Rotation must never destroy a live sandbox or sole snapshot copy.

**Diagrams updated:** C3 managed-service components and SB10 safety edges.

## 2026-11-08: a fork retried after 503 reuses the snapshot still being assembled

**What changed**

- E2B fork (`internal/platform/project/e2b_snapshots.go`): a fork whose snapshot is not ready within 60 s keeps it
  (template field `forkUntil`, 5 minutes from the request) and answers 503. A fork of the same sandbox in the same
  project before then takes that snapshot instead of capturing again, waits up to 60 s for it, and forks from it once
  the image is ready; the snapshot then goes. A failed assembly still drops it. Job `sandboxBuildSync` drops a fork
  snapshot past its window that no retry took (also after an API restart).
- `tests/e2b-compat/pause.py`: fork and create from a snapshot are retried on `ServiceBusyException` (5 tries with
  backoff); autoPause is drilled without autoResume (the SDK refuses it with `mode: "filesystem"`), and the
  autoResume wake separately, with `on_timeout: "pause"` and no mode.

**Why**

- The strict lab drill (stock SDK 2.63, main 5df7f4e) on a site where a snapshot image takes about 66 s to assemble:
  fork answered 503 after 60 s, as designed, but every retry took a new snapshot that again needed 66 s, so a fork
  could never succeed there. A create from a named snapshot already converged (a retry takes no new snapshot).

**Diagrams updated:** none; no container or component changed.

## 2026-11-08: sandbox pause answers before its image is ready; isolation lets workers reach their VM firewall endpoints

**What changed**

- E2B pause (`internal/platform/project/e2b_snapshots.go`): answers 204 once the capture is in the bucket, with the
  paused record in state `pausing`; the sandbox keeps running and billing, held past its own end if needed, until a
  background step sees the image pushed, then it is deleted (`stopping`, then paused). A failed assembly drops the
  pause and gives the sandbox its own end back. A connect, resume or wake during `pausing` waits up to 45 s, then
  cancels the pause and answers with the running sandbox. Fork and create from a snapshot wait at most 60 s from the
  request for the snapshot, then answer 503. A list shows a pausing sandbox once, as paused.
- `sandbox.Service.ShutdownAt` sets a sandbox's cluster deadline outside the timeout limits (the pause's hold).
- Cluster isolation (`internal/cloud/pve/network.go`, C4.1 rule 6): a TCP `ACCEPT` from `sandboxCidrs` to each
  `address:port` the workers' VM firewall opens (registry, build bucket, portal), above the sandbox `DROP`s; the
  isolation gate checks them as part of the exact set. Build and snapshot pods give up a connect after 10 s.
- `tests/e2b-compat/pause.py`: the strict pause, resume, fork and snapshot drill with the stock SDK.
- Review fixes: a pause records its sandbox only if it still exists and is not being deleted, and a kill of a running
  sandbox takes the pause lock (a kill during the capture no longer comes back as paused with a billed snapshot). The
  `pausing`/`stopping` lease is 2 minutes, renewed by the pause's background step; job `sandboxAutoPause` takes over
  one whose lease lapsed (API restart) and finishes it. A resume that drops a stale pause gives the sandbox the
  timeout asked for. A failed pause leaves the sandbox at least 5 minutes. A sandbox endpoint in the tenant pool or a
  blocked range is not opened.
- Re-drive fix: the pause's driver follows the image until it is pushed or its build fails, with no deadline from the
  pause's start (a re-drive past 15 minutes read the build with an expired context, took it as gone and dropped a
  ready snapshot, losing the files of a sandbox the cluster had removed at the hold). A failed store or cluster read
  is retried, not taken as a gone snapshot; a failed delete of the original gives the pause up only if the original
  still exists. The native cloud-resource delete of a sandbox ends a pause under way and deletes under the pause lock,
  like the E2B kill (it could otherwise be finished as paused).

**Why**

- The stock E2B SDK cuts every REST call at 120 s (60 s `request_timeout`, applied by pyqwest as read plus write).
  Pause, fork and create from a snapshot waited for the image inside the request and were cut at exactly 120 s on the
  lab, so the client lost a pause that went on, or a fork it could not see.
- The lab's build bucket sits on a management network. The isolation's sandbox `DROP` (before the exit node's SNAT)
  dropped the workers' traffic to it although their VM firewall opened it, so no snapshot or template build could
  fetch its input: each connect took 127 s to time out, four attempts per pod.

**Diagrams updated:** none; no container or component changed (C4.1 rule text in c4-code.md updated).

## 2026-11-08: sandbox-init runs the init only as PID 1 without arguments

**What changed**

- `cmd/sandbox-init`: an exec with an argument it does not know, or with no argument from a process that is not
  PID 1, now exits 2 with the reason. Before, both ran a second init inside the running sandbox.

**Why**

- Found in the SB7 lab drill: the portal asked a node with an older runtime for `sandbox-init -snapshot` (pause).
  The old binary did not know the flag and started a second init, which removed the ready marker and looped on
  envd's port; the pause failed after 126 s as it should, but the sandbox stayed unready (503 from the proxy) until it
  was killed. The same happens to any future mode on a node node-setup has not rolled yet.

**Diagrams updated:** none; no container or component changed.
## 2026-11-08: C2 caught up with the code: token-authenticated surfaces and the delivery pipeline

**What changed** (documentation only; no code or behaviour change)

- C2 API surfaces: rows for `/e2b/**` (a project's `e2b_` sandbox API key), `/api/v1/internal/sandboxes/**` (the
  sandbox provider's `proxyWakeToken`), `/api/v1/nocloud/**` (per-VM seed token) and `/api/v1/console/**` (console
  token). The debug-trigger row says `gen-hmac-key` needs debug triggers, not just the scheduler.
- C2 trust boundaries: the same three credentials as rows.
- C2c build and delivery: `docker.yml` also builds sandbox-proxy and sandbox-runtime; `sandbox-templates.yml` and the
  `proxmox-kamaji-cluster` chart are drawn; the K8s node template pipeline is Implemented (`k8s-node-templates.yml`),
  as the container table already said. The container table gains the kamaji chart row and the full workflow list.
- C4 code: the billing registry row lists every provider `cmd/api/main.go` registers (image, share, sandbox, sandbox
  snapshot were missing).

**Why**

The C4 maintenance rule wants every trust boundary in C2. Four public-prefix surfaces that authenticate with their own
token were only in `pkg/auth`, and the delivery diagram still showed the K8s node template pipeline as Planned while
the table and the workflow said otherwise.

**Diagrams updated:** C2 (API surfaces, C2c, container table, trust boundaries), C4 (billing registry row).

## 2026-11-08: MCP tool audit: dispatch skips the azp binding, 30 new tools, route test

**What changed**

- MCP tool calls run on a context marked by `auth.WithMCPDispatch`; `verifyJWT` skips the `azp` binding for it, as it
  already did on `/mcp`. The admin console's `azp` check (`adminContext`) and the Admin API gate are unchanged.
- Client toolset: `list_project_services`, `list_public_images`, `create_server`, backups (`list_backups`,
  `create_backup`, `restore_server_backup`, `delete_server_backup`), snapshots (list, create, rollback, delete),
  `enable/disable_server_protection`, Kubernetes (list, create, `get_kubeconfig`, `set_kubernetes_node_groups`,
  `upgrade_kubernetes_cluster`), databases (list, create), DNS (zones, records list/set/delete),
  `list_sandbox_templates`, `get_sandbox_access`. Admin toolset: `test_proxmox_connection`, `test_object_store`,
  `get_sandbox_release_gates`, `list_billing_profile_suspensions`.
- A tool row can fix a param's value (`param.fixed`, left out of the input schema) and force `destructiveHint`
  (`toolDef.destructive`); `test_` tools are annotated read-only.
- `TestMCPToolRoutesExist` (`internal/server`) matches every tool's method and path against the app router.
- A param can list its allowed values (`param.enum`: schema `enum`, refused at dispatch otherwise); `server_action`
  takes only REBOOT, HARDREBOOT, START and STOP.
- The sandbox create's autoPause and autoResume checks (autoResume needs autoPause; autoPause needs snapshot storage;
  autoResume needs the provider's wake token) moved from the E2B create to `pauseRefusal`, which
  `createSandboxResource` runs for both creates, so the console and `create_sandbox` get them too.

**Why**

- `/mcp` accepts a token from any client of a known issuer (an MCP client registers its own), then every tool re-sent
  that token to `/api/v1`, where the `azp` binding refused it: OAuth sign-in passed the gate and failed on every call.
  Skipping the binding only for the token `/mcp` already accepted adds no new way in; operator tokens still need the
  admin client, so operators use an API key, as the docs now say.
- An action tool whose action string the agent typed could be told to run another action than its name and its
  annotation say; a fixed value cannot.
- No test tied the tool rows to the router, so a renamed route would only have shown when an agent called it.
- `server_action` is annotated non-destructive but its route also runs DELETE_BACKUP, ROLLBACK_SNAPSHOT and
  RESTORE_BACKUP, which have their own destructive tools; free text let an agent reach them under the wrong hint.
- `create_sandbox` advertises autoPause and autoResume through the native create, which skipped the E2B create's
  checks: an autoPause sandbox where pause is off would be deleted at its timeout with its files, and an autoResume
  one with no wake token would never wake.

Diagrams: none changed (no new container or component).

## 2026-11-08: Quick start guide

**What changed**

- New `docs/quickstart.md`: a use-case table (laptop with Docker Compose, one Proxmox node, production `allinone`,
  `split` with managed Kubernetes, databases and sandboxes, the Helm chart alone), and per path the components it
  deploys, the inventory keys to fill, the command, the first login and the first steps after install. Also the
  Standard and Full storage tiers, the add-ons (GPU, PowerDNS, load balancers, backups, per-organization SSO,
  payments), what each installer stage installs and which key switches it, and the common first-install failures.
- The README's Quickstart section now points at it with a short path table.
- The Helm path (E) uses the chart's `values-quickstart.yaml` (PR #127) for a cluster without operators and keeps
  the operator install as the production path; it also says how to build and load the images until a release is
  published.

**Why**

- A new operator had to read `docs/install.md` end to end, plus `defaults.yml`, to learn which keys a single node or
  a split site needs. The Helm instructions also missed the `helm repo add` lines `helm dependency build` needs on a
  clean machine.

**Diagrams updated:** none; no container or component changed.

## 2026-11-08: PR #127 Helm quickstart install and first-run seed Job

**What changed**

- Chart `proxcloud` 0.3.29: `values-quickstart.yaml` installs api, ui, admin, PostgreSQL and RabbitMQ (bitnami
  subcharts) and Keycloak with its realms on a cluster with no operators, reached by port-forward on fixed localhost
  ports; secrets come from `--set` flags only.
- New hook Job `seed` (post-install, post-upgrade; PostSync under ArgoCD): the api image runs psql with the api's own
  DSN and writes `seed.platformConfiguration` and `seed.billingConfiguration` into `platformConfiguration` and
  `billingConfiguration` only while those tables are empty, under the same advisory lock `internal/pgdoc` takes to
  create a table.
- `keycloakConfigCli.adminPassword` is rendered into the Secret `<fullname>-keycloak-admin`; the quickstart's Keycloak
  takes its bootstrap admin password from it.
- New values `api.defaultRegion`, `api.billing.timeout`, `api.mail.businessName` (env the API already read).
- The chart README lists the two `helm repo add` lines `helm dependency build` needs, and how to build and load the
  api/web/admin images when the chart's `appVersion` is not published on ghcr.io yet.

**Why**

- A Helm install needed both operators in place, a realm made by hand and the seed run by hand before billing worked;
  the owner asked for a one-command Helm install beside `docker compose up`.
- The seed writes into empty tables only, so the operator console and `deploy/seed/seed-operator.py` stay the owners of
  those documents and no upgrade rewrites them.

**Diagrams updated:** c2-containers.md (PostgreSQL row)

## 2026-11-08: Sandbox pause, resume, snapshots and fork of the filesystem; autoPause and autoResume (SB7)

**What changed**

- E2B `pause`, `resume`, `snapshots` (create, list), `fork`, `connect` of a paused sandbox, `state=paused`, and the
  create's `autoPause` and `autoResume`, ported from the earlier implementation
  (`internal/platform/project/e2b_snapshots.go`). Filesystem only: `memory: true` and `autoPauseMemory: true` answer
  400.
- New component, pause and snapshots: `sandbox-init -snapshot` (`cmd/sandbox-init/snapshot.go`) writes a gzip tar of
  what changed since boot; the portal uploads it to the template build bucket and reads it as a whole archive while it
  uploads; a pod in `sbx-build` `crane append`s it onto the sandbox's image and pushes it with the project's build
  account. A snapshot is a `kind: snapshot` record in `sandbox_templates`; a paused sandbox is its saved CR in the new
  table `sandbox_paused`, every transition under a per-id transaction lock and a lease.
- Resume and fork count against the provider's limits and the project quota like a create
  (`Service.ResumeSandbox(..., quota)`).
- Job `sandboxAutoPause` (every minute) and debug trigger `run-sandbox-autopause`.
- The wake endpoint resumes autoResume sandboxes: the bearer must be the paused sandbox's own provider's wake token, and
  the client's envd or traffic token the sandbox's own; a suspended project or profile answers 403. Stage 56 sets
  `PROXCLOUD_WAKE_URL` (`sandbox.proxy.wake_url`) and rolls the proxy on a change; stage 60 writes
  `config.sandbox.portal` (`sandbox.proxy.wake_address`), and the workers' VM firewall admits that address and port
  (`SandboxFirewall.PortalAddress`).
- Billing: a stored snapshot is a `SANDBOX_SNAPSHOT` row per GB-hour from ready to resume, kill, template delete or
  teardown. Teardown archives those rows and forgets paused sandboxes; the registry prune keeps the images paused
  sandboxes ran.
- Build and snapshot pods request 2 GiB of ephemeral storage under the 30 GiB limit; Zot accepts Docker manifests
  (`compat: [docker2s2]`).

**Why**

- E2B users pause sandboxes to stop paying for compute and come back to their files; until SB7 an autoPause sandbox
  had to be refused, since it would have been deleted at its timeout.
- gVisor in a pod has no memory snapshot. Serving a memory pause as a filesystem pause would drop processes the caller
  asked to keep, so it is refused.
- The earlier attempt's lab drill had a 191 MB capture that crane could not read; that pause had already deleted the
  sandbox, which was lost. The archive is now checked while it uploads, and a pause deletes the sandbox only once its
  image is pushed: a failed assembly (crane, the registry, the 30 GiB disk limit) then fails the pause and the
  sandbox runs on, instead of a 204 followed by lost files. autoPause runs four pauses at a time, since each now waits
  for its image.
- A named snapshot taken again keeps only its newest ready build, so the stored size billed is that of the one image
  kept (keeping older builds kept their images unbilled). The size is the bytes counted while the archive is checked,
  not a HEAD that could fail and bill 0.
- The wake checks the location's token before the sandbox's own, and a resume gives up before its lease lapses, so a
  second caller cannot take over a resume still starting the sandbox.
- Concurrent connects of one paused sandbox (an SDK retry, autoResume traffic) must start it once.
- A resume or fork that skipped the quota would let a project exceed its sandbox caps by pausing and resuming.
- D9: suspension keeps the tenant's files but must not let them run; the wake answers 403 so the proxy does not keep
  retrying for the length of the suspension.
- A build pod with a limit and no request requests the limit; no 30 GB worker has 30 GiB allocatable (seen on the lab:
  Pending, "Insufficient ephemeral-storage"). `crane append` keeps a Docker base image's manifest type, which Zot
  refuses without the compat setting.

**Diagrams updated:** C3 (pause and snapshots component and its flows, the proxy's wake), C4 (the fence rule for the
portal).

## 2026-11-08: MCP gateway template for E2B's `Sandbox.create(mcp=...)` (SB8)

**What changed**

- New component inside sandboxes: `cmd/mcp-gateway`, ported from the earlier implementation with its catalog
  (`catalog.json`, 47 servers, regenerated by `gencatalog.py`). The E2B SDK starts it in a sandbox of the new built-in
  template `mcp-gateway` (`deploy/templates/mcp-gateway`: the base, the gateway, the package servers pinned), and talks
  MCP to `:50005/mcp` through `sandbox-proxy` with a bearer only it and the sandbox hold.
- The E2B create accepts `mcp` when it is an object (was refused with 400) and drops it after that check: not stored,
  not logged. The OpenAPI document says so.
- The package servers run on a Node 22 of their own in the image; the base image's Node 20.9 could not start `brave`,
  `firecrawl` or `mongodb`. `mcp-gateway pull`, the template's build check, now starts every package server and asks
  for its tools instead of only looking for the binary.
- `build.sh` builds `mcp-gateway` with the other templates (repository root as the `src` context); the
  `sandbox-templates` workflow also runs on changes under `cmd/mcp-gateway/`, and pins the image in
  `deploy/templates/images.yaml` like the others, from where the registry stage copies and pre-pulls it.
- `tests/e2b-compat/mcp.py` (gateway, tools, auth, keyed server with a dummy key, unknown server) and `catalog.py`
  (every built-in template, `mcp-gateway` included).
- Review fixes: a remote server's headers (its key) go only to that server's own scheme and host, so a redirect
  elsewhere does not carry them; each package server gets a short allowlist of the gateway's environment plus its own
  config instead of all of it (no `GATEWAY_ACCESS_TOKEN`, no other server's key, no sandbox `envVars`); vendor
  telemetry off by default (`DO_NOT_TRACK`, desktopCommander's, mongodb's and onnxruntime's switches); the build check
  runs with a throwaway `HOME` and `TMPDIR`, so the image no longer ships a desktopCommander client id with telemetry
  on, a mongodb log or an onnxruntime device id and session file, and the build fails if such state is left.
- From the lab drill: `sandbox-init` drops `KUBERNETES_*` like `PROXCLOUD_*` (the kubelet sets
  `KUBERNETES_SERVICE_*` and `KUBERNETES_PORT*` in every container even with service links off), and the API refuses
  `envVars` with either prefix. `node-setup` writes runsc `allow-suid = "true"` by default (`RUNSC_ALLOW_SUID`,
  installer `sandbox.allow_suid`), so `sudo` works in sandboxes as on E2B; it failed in the base template with
  "effective uid is not 0 ... nosuid". Security effect in `docs/managed-sandbox.md` §1.4. Default chosen pending the
  owner's decision (the earlier implementation's owner approved it for E2B parity).
- Second review, docs only: `docs/managed-sandbox.md` §9.2 now says the per-server allowlist is environment hygiene,
  not a boundary (every package server runs as root like the gateway and can read the config, the token file and the
  gateway's `/proc/<pid>/environ`); §1.4 says `allow-suid` is node-wide, so it also covers the gVisor build and
  snapshot pods; §5.3 says sandbox-init's own `/proc/1/environ` keeps the variables it strips for its children;
  §9.2 lists desktopCommander's feature-flag fetch and build-time install event, which no switch turns off.
- Diagrams: C3 sandbox diagram (node `mcp-gateway`, edge from `sandbox-proxy`), C3 component row "MCP gateway".

**Why**

E2B's MCP support is part of the SDK surface agents use, and the earlier implementation had it; without it every
`mcp=` create failed. The gateway runs inside the tenant's own sandbox, so the servers' keys never pass through the
platform: they go from the SDK to the sandbox, the portal refuses only a malformed field and keeps nothing of it, and
the sandbox carries no platform credential the servers could read. The Node and build-check changes come from the lab
run: three catalog servers were installed but could not start on the base image, which only a start shows.
The review fixes keep each key with the one server it is for: a third-party server that redirects, or a package server
that reads its environment, saw the tenant's key or the gateway's bearer before. State written by the build check
would have given every sandbox the same vendor client id with analytics on. The `KUBERNETES_*` variables pointed
sandbox code at the cluster's API server for nothing, and sudo is what E2B templates and agents expect to work.
The second review's doc changes stop §9.2 promising more than the code does: a reader planning around "a server never
sees another server's key" would be wrong, since any server can read the root-only files as root.

## 2026-11-08: Custom sandbox templates: build pods, a build account per project, pruning through the OCI API (SB6b)

**What changed**

- The E2B v3 template API (`POST /e2b/v3/templates`, files, builds, build status, tags, delete) and the template list,
  alias and get with the project's own templates, ported from the earlier implementation
  (`internal/platform/project/e2b_templates.go`, `e2b_template_base.go`); records in the new table `sandbox_templates`.
  A ready build is a template for the project's sandboxes on the E2B and the native create.
- New container: the template build bucket (`sandbox.registry.builds_bucket`, RustFS or RGW, its own service user),
  set on the sandbox provider as `config.sandbox.buildStorage` with its keys in the secret. The portal streams contexts
  into it (one PUT up to 16 MiB, else a multipart upload, `UNSIGNED-PAYLOAD` for both) and deletes a template's contexts
  with it.
- Build pods in namespace `sbx-build` on the sandbox cluster (`installer/playbooks/templates/sandbox-builds.yml.j2`,
  stage 55 (registry)): fetch with presigned GETs for the endpoint the nodes reach, base pulls, kaniko offline with no
  credential, crane push. Its own egress policy (internet minus private ranges, port 25 closed, the registry and the
  bucket alone), the registry CA for crane, the portal's Role (pods, ConfigMaps, `create`/`patch` on Secrets) and a
  `patch`-only Role for the account sync. The workers' VM firewall gets one rule more for the bucket
  (`SandboxFirewall.BuildStorageAddress`, Proxmox `config.sandbox.buildStorage`, stage 60).
- Registry accounts: the shared `push` account is gone; the account sync writes `build-<project>` (reads `library/**`
  and `t/<project>/**`, writes `t/<project>/**` only) into `sbx-build` as `registry-build-<project>`, and a static `gc`
  account reads and deletes `t/**`. The sync's admission policy allows that one Secret name pattern in `sbx-build`.
- `fromImageRegistry` types `registry` and `gcp`; `aws` is refused, so the AWS SDK's ECR client is not a dependency.
- Jobs `sandboxBuildSync` (every minute) and `sandboxRegistryPrune` (hourly, `config.sandbox.registryPrune`, the `gc`
  account, `config.sandbox.registryCa`); debug triggers `run-sandbox-builds` and `run-registry-prune?grace=`.
- Console: a Templates tab on the Sandboxes page (`/project/{id}/sandbox-templates`: list, build logs, delete); the admin
  sandbox provider form edits the build bucket, the prune mode, the gc account and the registry CA.
- Project teardown deletes the project's templates, their build pods and contexts, and its images under
  `t/<project>/` with the `gc` account (following `registryPrune`).
- The build limit (2 per project) is counted and the pod started under a Postgres advisory lock per project; a
  template keeps its last 10 builds plus any a tag names or that is building; a project's contexts are capped at
  10 GiB (oldest past the presigned URL's lifetime evicted first, else 507), and the upload URL and the upload pass the
  usage gate. The build namespace's Cilium policy has an explicit empty ingress rule (default deny).

**Why**

- Tenants of the E2B SDKs build their own templates (`Template.build`); without it only the operator's built-in
  templates existed.
- EE pushed every tenant's builds with one robot that could overwrite any tenant's template; a build pod is customer
  code on a shared node, so its push credential is now its own project's, and no account but `admin` writes
  `library/**`.
- EE's build pods downloaded their contexts from the portal. The workers reach the object store (layer 1 can open one
  address) but not the portal, which the plan keeps off the CPU workers (only the system node reaches it, for wake);
  presigned GETs need no portal route and no bucket key on the cluster.
- EE pruned through Harbor's API; Zot speaks the OCI distribution API, with which the same live-set rules apply.
- ProxCloud runs on Proxmox only, so an AWS SDK client for one `fromImageRegistry` flavour was not worth carrying; an ECR
  token used as a registry password covers the same images.
- Review of the first cut: 10 failed build requests trimmed away the build `default` named (the template stopped
  resolving and the prune lost its image); parallel build requests all passed the count and started 3 pods for a
  limit of 2, so one project could fill the quota every project's builds share; contexts had no cap and no usage gate
  on the object store that also holds the registry's blobs; and the prune treats a repo of an unknown project as
  foreign, so a deleted project's images would have stayed forever. `ingress: []` is no ingress section to Cilium,
  and `sbx-build` carries no tenant label for the cluster-wide rule.

**Diagrams updated:** C2 (template build bucket, build pod flows), C3 (custom templates component, the account sync's
accounts), C4 (the fence rule for the bucket).

## 2026-11-08: The sandbox registry: Zot on the services cluster, a pull account per project, pinned built-in images (SB6a)

**What changed**

- New container: the sandbox registry, Zot (minimal build, digest-pinned) on the services cluster behind one MetalLB
  address, TLS from a CA of its own, its blobs in a bucket of the object store: RustFS on Standard, RGW on Full,
  through a service user limited to that bucket (`files/rustfs-bootstrap.py` learned plain buckets, RGW and external
  stores). Installer stage `55-sandbox-registry.yml` (also in `make upgrade`), on when `sandbox.registry.address` is set.
- `sandbox-proxy registry-users` (`internal/sandboxproxy/registryusers.go`), a native sidecar of Zot: from the sandbox
  cluster's tenant namespaces it writes Zot's users and access rules (`pull-<project>` reads `library/**` and
  `t/<project>/**`, `pull-nodes` reads `library/**`, `push` writes `t/**` only, `admin`) and one pull Secret per project;
  project passwords are an HMAC of the account name, so nothing is stored per project. Its identity on the sandbox
  cluster (`deploy/sandbox-cluster/registry.yaml`) lists namespaces and touches only the two Secret names it writes.
- Sandbox pods name their project's Secret for every image on the registry's host; the portal no longer copies one
  shared pull Secret into every project namespace.
- Built-in images: `deploy/templates/` (from the earlier implementation, minus `mcp-gateway`; the desktop image without
  Chrome and VS Code), the `sandbox-templates` workflow (ghcr, then a pull request that pins the digests) and the pin
  file `deploy/templates/images.yaml`. Stage 55 copies every pin and the release's sandbox-proxy in by digest, pre-pulls
  them on every node (`template-prepull`) and makes containerd trust the registry CA for that address alone
  (`registry-trust`). Stage 56 runs sandbox-proxy from the copy; stage 60 resolves `pin:<name>` templates and sets
  `templateRegistry` and `imageAdoption`.
- Image adoption ported: job `sandboxTemplateAdopt` (`project.AdoptSandboxImages`) moves a built-in template to the
  digest the prepull runs once every node has it, with a compare-and-set write (`externalservice.SetTemplateImage`) and an
  audit event. The admin form edits `templateRegistry` and `imageAdoption`.
- Layer 1: `config.sandbox.registry` adds one rule to the workers' generation, TCP out to the registry's address and
  port (`SandboxFirewall.RegistryAddress`).

**Why**

The nodes pulled templates straight from public registries (plan §6: until SB6a), and the earlier implementation gave
every tenant namespace the same pull account, so a node escape read every tenant's templates and a compromised build
could overwrite another tenant's. A registry of the platform's own, with one account per project and a push account
that cannot touch `library/**`, closes both; digest pins plus prepull plus adoption mean a new image reaches a sandbox
only after every node has it. Zot over distribution: garbage collection while serving, access rules reloaded without a
restart (measured), one binary. The account sync derives the accounts from the cluster instead of the portal writing
registry config, so the portal gains no credential for the services cluster.

**Review fixes (same PR)**

- `registry.yaml` gained the ValidatingAdmissionPolicy `sandbox-registry-users-pull-secrets-only`: the sync's identity
  may create and update only `kubernetes.io/dockerconfigjson` Secrets named `sandbox-template-pull` in `sb-*` and
  `sandbox-registry-nodes` in `sandbox-system`. Why: RBAC cannot limit `create` by name, so the role alone could create
  a service-account-token Secret named `sandbox-template-pull` in `kube-system` for a controller's account and read the
  token the controller filled in, a cluster-admin token held on the services cluster.
- After a sidecar restart with a failing namespace list the sync keeps Zot's config on disk instead of writing the
  static users only. Why: every project's pulls failed until the next successful list.
- `sandbox-proxy` refuses an unknown subcommand. Why: an image older than `registry-users` served instead, and Zot
  waited for a readiness that never came with no cause in the log.
- `registry-trust` runs busybox pinned by digest (D11); the workflow builds pull requests in a job with a read-only
  token and pushes only from the other events.

**Merged with SB5 (main after #120)**

- The registry design is section 6 of `docs/managed-sandbox.md` now, after SB5's E2B section 5; every reference moved.
  Why: SB5 landed first and its own section and references kept the number.
- One sandbox provider from stage 60 carries both sides' keys: `envd` and `domain` (SB5) and the `pin:` templates,
  `templateRegistry` and `imageAdoption` (SB6a). The admin form edits all four, and a save keeps the template fields
  the one-line list cannot show (SB5's `mergeTemplates`). Why: either branch alone wrote a provider without the other's
  keys, and a form missing a field would have saved it empty.
- `deploy/templates/build.sh` builds every built-in template (SB6a) with SB5's pinned ijavascript commit in the
  code-interpreter image.
- C1 and C2 say which images the sandbox nodes take from the registry (templates, `sandbox-proxy`) and that SB5's
  runtime image still comes from `images.registry`. Why: stage 55 (cluster) applies `node-setup` before the registry
  stage exists, so "every runtime image from the registry" was not true once both were merged.

**Diagrams:** C1 (ghcr row), C2 (the registry container, its edges to the workers and the object stores, table row),
C3 (registry component and edges), C4 (layer-1 rule list). Measured on the lab: docs/managed-sandbox.md §6.6. The
review fixes change no diagram.
## 2026-11-08: E2B-compatible API, sandbox API keys, envd in sandboxes, MCP sandbox tools (SB5)

**What changed**

- Portal: `/e2b/*` (`internal/platform/project/e2b.go`), mounted next to `/api/v1` and public in `pkg/auth`; its own gate
  reads `X-API-Key` (or a bearer) and resolves it to the key's creator in the key's project. Sandbox create (v1 and v2),
  list, get, kill, timeout, refreshes, connect, network, logs, metrics and the built-in templates, in E2B's shapes and
  `{code, message}` errors; anything else answers 404 "not supported". `GET /e2b/openapi.json` is E2B's own document
  cut to those routes (`e2b_openapi_gen.py`), guarded by `TestE2BOpenAPIParity`.
- Sandbox API keys (D8, `e2b_keys.go`, table `sandbox_api_keys`): `e2b_` + 40 hex, one project each, acting as the member
  who made it, stored as SHA-256 only; `GET/POST/DELETE /api/v1/project/{id}/sandbox-api-keys` and the client console's
  Sandboxes > API keys tab (key shown once, then a hint). The project service view says whether the location offers the
  E2B API (`sandboxE2B`); the admin sandbox provider form edits the sandbox domain and an envd switch separately
  (`config.sandbox.domain`, `envd`) and keeps a template's start and ready commands when the template list is edited.
  Teardown deletes a DELETED project's keys.
- In the sandbox: `cmd/sandbox-init` (PID 1: user, envd, `/init`, start and ready commands, localhost relay) and
  `deploy/sandbox-runtime.Dockerfile` (envd built unmodified from e2b-dev/infra e85b9f5 plus sandbox-init), installed on
  every node by a new init container of `node-setup`; stage 55 pins its image and leaves the init container out where
  there is no sandbox domain, CI builds `proxcloud-sandbox-runtime`. Sandbox pods get a readiness probe on the ready
  marker, which sandbox-init drops while a crashed envd restarts.
  Built-in templates `base` and `code-interpreter-v1`: `deploy/templates/` and `build.sh` (pushed by hand until SB6a's
  workflow); stage 60 registers a provider with a domain with envd on.
- MCP client toolset: `list_sandboxes`, `create_sandbox`, `exec_in_sandbox`, `get_sandbox_logs`, `set_sandbox_timeout`,
  `delete_sandbox` over the native routes.
- `tests/e2b-compat/compat.py`: the stock SDKs end to end (commands, streaming, files, preview URL, metadata, egress,
  cross-project key, code interpreter). `docs/e2b-compat.md`: what works, what differs and what does not, and why.
- C2: the sandbox cluster row had two sentences starting "Built by installer stage 55"; merged into one.

**Why**

- The plan's SB5 (§11, D8): the E2B SDKs are how agents use sandboxes, and a key must not reach every project of its
  user the way a PAT does, so keys are project-scoped and stored as hashes.
- `/e2b` is a second door to the same sandboxes. If it had its own create, a suspended project or a provider whose
  release gates fail would run sandboxes through it, so it calls the native gate, quota and create (proved by
  `test/integration/sandbox_e2b_test.go`, which fails with the gate calls removed).
- autoPause, autoResume, templates builds, snapshots and MCP are refused with 400 or 404 instead of being accepted and
  ignored: without pause (SB7) an autoPause sandbox would be deleted at its timeout and its files lost.
- `PUT /e2b/sandboxes/{id}/network` is a mutation the native allow-list of a DISABLED project does not hold, so it is
  refused there too (409).
- envd with no access token serves every request, and a restarted envd has none until its `/init`; sandbox IDs are in
  every shared preview URL, so that window must not be reachable. Readiness, not just startup, now follows the marker.
- One domain field that also switched envd made every edit of an SB4 provider (domain, no envd) save an empty domain,
  which breaks its published ports; a site without envd would also pull a runtime image no release had yet.

**Diagrams updated:** `c1-context.md` (registry images), `c2-containers.md` (API box, sandbox cluster row), `c3-components.md` (sandbox diagram: E2B API and envd; E2B component row).

## 2026-11-08: Sandbox ports in public: sandbox-proxy with TLS, entry appliance, wildcard certificate, wake (SB4)

**What changed**

- `cmd/sandbox-proxy` + `internal/sandboxproxy`, ported from the earlier implementation: `<port>-<id>.<domain>` to the
  ready pod, envd's control routes refused, the traffic token enforced, wake on a miss. New: it terminates TLS itself
  (`SANDBOX_TLS_DIR`, the mounted Secret read again on renewal through `pkg/certfile`, moved out of `csigw`), waits
  for its first certificate while alive and not ready, and a `cert-sync` mode. The forwarded-host trust is gone.
  Manifest `deploy/sandbox-cluster/sandbox-proxy.yaml`, image `deploy/sandbox-proxy.Dockerfile` (CI builds
  `proxcloud-sandbox-proxy` with the other images).
- Installer stage 56 (`56-sandbox-edge.yml`, also in `make upgrade`): `sandbox-proxy` on a MetalLB address; on the
  services cluster an issuer (`acme` DNS-01 with RFC2136 to PowerDNS, `secret`, or `selfsigned`), the wildcard
  `Certificate` and `cert-sync`, which copies it to the sandbox cluster with a credential limited to one Secret; on
  PowerDNS the wildcard record and a TSIG key allowed to update that zone alone, from the services network; the entry
  appliance VM in its own pool. Stage 10: pool `proxcloud-sandbox-entry`, role `ProxCloudSandboxEntry` for the portal,
  `NoAccess` for the CSI and services CAPMOX tokens. Stage 60: the Proxmox provider's `config.sandbox.proxy`.
- Portal: the layer-1 group admits the entry appliance's vnet address alone to `sandbox-proxy`'s address
  (`SandboxFirewall.entryRule`); the sync keeps the appliance's firewall and HAProxy config (`pve.SandboxEntry`, the
  P8-1 render and push), touching only a VM tagged `proxcloud-sandbox-entry`. `POST /api/v1/internal/sandboxes/{id}/wake`
  (public path, a sandbox provider's `proxyWakeToken` as bearer; 404 for every sandbox until SB7). Native action
  `GET_ACCESS` returns a sandbox's host pattern and traffic token from the live CR.
- Fixed in stage 55 on the way: `-o jsonpath={.data.admin\\.conf}` lost its backslash to the command module's word
  splitting, so the wait for the sandbox cluster's kubeconfig could never succeed.
- After review, same PR: `sandbox-proxy` runs on a node group of its own. Stage 55 adds `sandbox-system` when a domain
  is set: its Machines carry `node-restriction.kubernetes.io/proxcloud-system` (new chart value `machineLabels`, which
  CAPI copies onto the Node) and the taint `proxcloud.io/sandbox-system`, which MetalLB's speaker tolerates (new chart
  value `proxmox.metallb.speakerTolerations`, empty for tenants, so their clusters render as before). The proxy
  requires that label, no preferred fallback. Chart 0.1.8. cert-sync copies only a certificate valid now and for
  `*.<domain>`. The entry appliance gets one-hour idle timeouts, and the sync touches it only in its own pool
  (`config.sandbox.proxy.entry.pool`), not merely when tagged. Stage 56 leaves the proxy's wake URL unset until SB7.

**Why**

- D15 (after SB0): a Gateway in the sandbox cluster needs kube-proxy replacement, which the chart's Cilium does not
  run, so TLS ends in the proxy and nothing else is added to the cluster that runs tenant code.
- D5: the key that changes DNS must not live where an escape could reach it, so cert-manager and the TSIG key stay on
  the services cluster and only the certificate crosses, with a credential that can write nothing else.
- D4: no Octavia; the P8-1 appliance already does TCP passthrough with a checked reload through the guest agent, so
  the entry reuses it rather than adding an ingress. Its pool gets its own narrow role because it faces the internet.
- A tenant using the native API could set `allowPublicTraffic: false` but had no way to learn the token it then needs.
- The system node group: a kubelet may read the Secrets of the pods bound to its node, so a gVisor escape on a node
  with a proxy pod would get the wildcard key (every tenant's host) and the wake token. Only a label the kubelet cannot
  set itself (NodeRestriction) can be trusted to mean "no tenant code here", hence CAPI's label propagation rather than
  `--node-labels`. A tag on a VM is set by whoever owns the VM, and the portal's `/pool` role reaches tenant VMs, so
  the appliance check needs the pool as well. The wake URL stays off because nothing can be resumed before SB7 and
  every unknown host would otherwise cost an unauthenticated call to the portal.

**Diagrams updated:** `c1-context.md` (sandbox ports, PowerDNS and ACME roles, images), `c2-containers.md` (entry
appliance, certificate copy), `c3-components.md` (sandbox proxy, entry appliance, `GET_ACCESS`, diagram), `c4-code.md`
(layer-1 entry rule).
## 2026-11-08: SB3 live drills: sandbox minutes, isolation repair, stage 60 registers the sandbox provider

**What changed**

- `pricing.advanceWatermark`: a row whose `DeletedAt` is past the charge's cycle timestamp moves the item's watermark to
  `DeletedAt`, not to the cycle.
- `billingresource.SandboxProvider`: the billed window starts at the minute `runningSince` falls in
  (`Info.CreatedAt` and `billedEnd` cut to the minute), and is the runtime rounded up to whole minutes, at least one.
- `pve.ReconcileIsolation`: the rule compare includes `enable` (`sameLiveRule`), and the rules it writes carry
  `enable: 1`, so a DROP or project ACCEPT disabled by hand is rewritten by the next sync.
- `project.sandboxServiceOf` falls back to a `DISABLED` sandbox provider, so a create that names no service is
  refused with 503 instead of being handed to Proxmox.
- Installer: stage 55 also keeps `generated/sandbox-portal.kubeconfig` (service account `proxcloud-system/proxcloud`);
  stage 60, with `sandbox.enabled`, registers `<provider> Sandboxes` with it, stored `DISABLED`, templates and limits
  from `sandbox.templates` / `sandbox.limits`, first run only.

**Why**

- Measured on the lab with the SB3 RBAC applied: every ended sandbox paid one minute more than its runtime rounded up
  (39 s → 2, 4 min 50 s → 6, 9 min → 10). The engine bills every clock minute an item spans, so a window starting
  mid-minute spans one more; and a row ended after a charge's cycle but before that charge ran was rated to its end
  with the watermark left at the cycle, so the next charge rated that last unit again. The second holds for hourly
  resources deleted in the minutes between the hour and the hourly charge.
- With the sandbox provider `DISABLED`, an API create naming no service answered "SANDBOX is not supported on a Proxmox
  provider" (400), which reads as a product limit rather than an operator switch.
- The SB3 review found the reconcile blind to `enable`: the isolation gate refused a disabled rule, but nothing
  repaired it, so sandboxes stayed refused until someone re-enabled it by hand.
- A site with `sandbox.enabled` had the cluster and the fenced workers but no provider for tenants to use: the
  operator had to build the kubeconfig and the provider by hand. Stored `DISABLED` because the fence gate needs a sync
  to have fenced the workers first, and a re-run must not undo an operator's status or template edits.

**Diagrams updated:** `c2-containers.md` (sandbox cluster), `c3-components.md` (billing resources), `c4-code.md`
(isolation invariants).

### Review fixes (same PR)

**What changed**

- `pve.deleteClusterRule` finds the rule to delete with `sameLiveRule` (enable flag included), not `sameRule`.
- Stage 60 skips the sandbox provider when any row has `config.provider: sandbox`, not when a row has the default name.
- Stage 60 takes the site's `sandbox.templates` as given; the default `python` template is used only when the site
  sets none.

**Why**

- With `sameRule`, a disabled copy of a project ACCEPT above its enabled twin made the sync delete the enabled twin
  (the lowest match) and keep the disabled copy, cutting traffic inside that project until the next sync.
- An operator who renamed the provider in the console got a second, disabled one on the next installer run.
- `pc` is a recursive merge of the defaults and the site, so a site could not drop the default template.

**Diagrams updated:** `c4-code.md` (isolation invariants).

## 2026-11-08: Sandboxes reach tenants: native API, billing, quota, suspension, release gates (SB3)

### Review fixes (same PR)

**What changed**

- New minutely job `sandboxStateSync` (`syncjob.RunSandboxState`, at :10, debug trigger `run-sandbox-state`): every
  enabled project's `SANDBOX` rows off the sandbox cluster before the minutely charge. `EXEC` records the read too.
- `sandbox.SyncProvider.KeepUnread` (replacing the never-called `Linger`): a vanished sandbox's row ends
  (`sandbox.MarkVanished`) and is archived; a row never read Ready gets its start from its last pending read
  (`seenPendingAt`, stamped on every read of a PENDING sandbox) or its create. Delete, the suspension's delete and the
  teardown read each sandbox first and end rows whose sandbox is already gone the same way.
- `sandboxUsageGate` refuses create, `EXEC` and `SET_TIMEOUT` on a `DISABLED` provider (503).
- `pveQuotaLimits` and the Proxmox bootstrap ignore the sandbox quota keys when deciding whether a project has its own
  Proxmox quota.
- `deploy/sandbox-cluster/rbac.yaml`: a ValidatingAdmissionPolicy refuses the NodeRestriction probe identity every
  write that is not a dry run.

**Why**

- A sandbox row was the create payload until a list, a delete or the 15-minute sync read it; exec did not. An API
  user who only ran `EXEC` and let each sandbox time out before the next sync was never billed (archived `PENDING`,
  nothing to rate). The charge does rate archived rows, so the fix is that every ended row carries a start.
- Disabling a provider is how an operator takes it off: tenants could still create through `x-service-id`.
- A project attached to sandboxes before Proxmox held only sandbox keys, which counted as "its own quota" and made
  every Proxmox key unlimited.
- The impersonation grant allows a real Node create, not only the dry run: a leaked firewall token could register
  a Node and receive tenant pods until the fence gate noticed.

**Diagrams updated:** `c3-components.md` (sync job, billing resources).

**What changed**

- Project cloud routes serve `SANDBOX`: create (a create with no service goes to the project's sandbox provider),
  list with a live read-through, delete, and the actions `EXEC`, `GET_LOGS`, `SET_TIMEOUT`, `REFRESH`
  (`internal/platform/project/cloud_sandbox.go`, `cloud_writes.go`). Bootstrap attaches projects to a sandbox
  provider (`BootstrapSandboxOnto`: namespace, ResourceQuota); the sync job keeps the rows (`syncSandboxService`).
- Billing types `sandbox` (per minute, `vcpus`, `ram_gb`, `gpu_count`, `gpu_model`) and `sandbox_snapshot` (GB-hour)
  in the billing registry and the catalog; seed rules marked PLACEHOLDER, in `seed-operator.py`'s `RULE_TYPES`. An
  archived row keeps its provider's earlier end (`archivedEnd`).
- Quota keys `sandboxes`, `sandboxVcpus`, `sandboxRamMb`, `sandboxGpus` (D10 defaults 5 / 16 / 32768 / 0,
  provider default `config.sandbox.projectQuota`), checked against the live sandboxes and carried by the namespace
  ResourceQuota; admin quota validation and the Quota tab take them.
- Suspension gate: create, `EXEC` and `SET_TIMEOUT` pass `refuseNewUsage` (and the allow-list from #84) and the
  billing profile check; `billingCloudSuspender` deletes a suspended project's running sandboxes (D9).
- Project teardown deletes sandboxes and the `sb-<project>` namespace first, and retries while either is left.
- Release gates: a sandbox provider names its workers' Proxmox provider (`config.sandbox.proxmoxServiceId`); the
  portal checks NodeRestriction (dry-run Node creates impersonating a node, `rbac.yaml` grants exactly that
  impersonation to the firewall service account), the isolation DROPs (`pve.CheckSandboxIsolation`) and the worker
  fence (`pve.CheckSandboxFence`, read-only). Admin create or update leaving a sandbox provider live answers 409 when
  one fails; every sandbox create, exec and timeout extension checks them again (cached a minute, fail closed).
- Consoles: client Sandboxes page (create, run command, logs, extend, delete); admin Sandboxes provider form, detail
  tab with a release check, project Quota tab keys.

**Why**

- The plan's release rule: no tenant may run code before layer 1 (SB2b), the suspension gate, quota and billing are
  all in place, so the routes, the gates and the money land in one phase.
- Each gate is checked by what it protects against, not by the configuration that should produce it: the
  NodeRestriction probe asks the API server with a node's identity (Kamaji's default list lacks the plugin, SB0),
  the isolation and fence gates read PVE with the portal's own token. A gate that stops holding later stops new
  sandboxes at the next minute.
- Sandboxes are disposable and bill by the minute: on a suspension, pausing them would keep capacity held for an
  unpaid account, so they are deleted (D9). A project's sandbox namespace outlives no project.
- An archived sandbox row was billed to its archive time, minutes after it ended (rows linger for the minutely
  charge).

**Diagrams updated:** `c2-containers.md` (sandbox cluster), `c3-components.md` (external services, sandboxes, billing
resources, sync job, diagram), `c4-code.md` (C4.3 keys, new C4.5, teardown sequence).
## 2026-11-08: SB2b follow-ups: host ICMP anti-spoof guard, CAPI off the sandbox pool, SNAT message

**What changed**

- New installer artefact `sandbox-icmp-guard.py` + a systemd service and one-minute timer
  (`proxcloud-sandbox-icmp-guard`), installed by stage 55 on every `pve` node when `sandbox.enabled`. It reads each
  local sandbox-pool worker's `ipfilter-net<N>` from PVE's cluster config and installs a host nftables chain
  (`table bridge proxcloud-sandbox`, prerouting priority -300, ahead of PVE's `vm-out`) that drops an ICMP/ICMPv6 error
  leaving a worker's tap whose source is not that worker's pinned address. Documented as layer 1b in `c4-code.md` C4.1
  and `managed-sandbox.md §2.6`.
- Stage 10 gives `capmox@pve!capi` (the services cluster's CAPMOX token) `NoAccess` on `/pool/<sandbox pool>`, a token
  row beside the existing `csi@pve` user row. Its identity check now reads each token's effective rights on the pool
  AND on every worker VM (`/vms/<id>`), not only `/pool/<pool>`: the portal must hold the guard role alone, the CAPI
  and CSI tokens nothing.
- `sandbox.network.snat`, left without a default on purpose, now fails stage 55 with a message that says exactly what
  to set and why (the sandbox's own egress address so abuse reports name it, not the nodes/portal/tenants; or `node`).
- Review follow-ups to the guard: it now drops any IPv4 frame from a worker's tap whose source is outside its
  `ipfilter` (not only ICMP errors); it reads every worker's config from `/etc/pve/nodes/*/qemu-server` and pins it on
  every node, tap present or not; `!` nomatch entries are excluded ahead of the allow list, and non-CIDR entries are
  skipped (fail closed) instead of breaking the ruleset; drop rules carry counters. Stage 55's check also requires the
  timer active and the last guard run successful, since the chain is legitimately empty before workers exist. New
  installer test `tests/test_icmp_guard.py`.

**Why**

- PVE's bridge firewall accepts ICMP error types in `vm-out`/`vm-in` before the per-NIC `ipfilter` and the DROP policy,
  so a worker could forge their source and reach PVE host input and node addresses off-path (lab, PVE 9.2). The VM
  firewall cannot close it; a host rule ahead of PVE's chain can. The guard mirrors `ipfilter`, so it drops nothing
  PVE's anti-spoof would allow, and the timer keeps it current well inside a new worker's 20-minute unsecured taint. It
  is the installer's and not the sync's because the portal's PVE token reaches the API only, never a node shell.
- Through `/pool` the services CAPMOX token inherited `ProxCloudCAPI` (clone, config, power, migrate) on the sandbox
  workers, VMs that run tenant code; it drives only the services and tenant clusters and never the sandbox, which runs
  through `capmox@pve!sandbox`. A stray per-VM ACL or a group grant would also be invisible to a pool-only check.
- The review found PVE's `pre-vm-out` also accepts conntrack established/related before `ipfilter`, so a forged packet
  matching a flow tracked on the node's bridge passes PVE whatever its protocol; pinning every source closes that too
  and is no stricter than `ipfilter` itself. A worker that live-migrates or HA-restarts already runs tenant pods (no
  unsecured taint), so the old "tap must be up here, config under this node" rule left it unpinned for up to a minute
  or more. One malformed `ipfilter` entry used to make `nft -f` fail for the whole node, leaving every worker there on a
  stale table.

**Diagrams updated:** `c4-code.md` (C4.1 layer 1b).

## 2026-11-07: SB2b review fixes: pin, datacenter switch, node addresses, re-taint

**What changed**

- `pve.SandboxPass`: a filter counts as pinned only when it holds one worker-range entry and that is a host;
  `StartSandboxPass` reads the datacenter firewall switch and, with it off, `Secure` still writes the worker's
  firewall but returns `ErrSandboxUnenforced` and never reports it Secured. New `Client.NodeAddresses`: every IPv4
  address on every node interface.
- `syncjob.reconcileSandboxWorkers`: the internet set also leaves out every node address and `config.sandbox.snat`;
  a node that does not report its networks stops the pass; the Node's address is passed to `Secure` only while the
  worker is tainted; a cleared worker that fails, reads back not in place or loses its pin gets the taint back.
- Stage 55: the SNAT dummy interface drops new connections to its address in `INPUT` (`pre-up`). Stage 60 writes
  `config.sandbox.snat`. `unsecuredMinAgeMinutes` defaults to 20.

**Why**

- A worker range like `.10-.20` covers as CIDRs ending in one host, which the pass read as a pin: every unjoined
  worker lost its own address and never joined.
- With the datacenter firewall off PVE applies no VM rule, yet workers read back in place and lost their taint.
- The deny list held only bridges with a gateway: a node's PVE API and ssh on another uplink, and the SNAT address on
  a dummy interface, stayed in the internet set, and the SNAT address put the exit node's API and ssh on a new,
  often public, address. Found by review, confirmed on the lab (pveproxy answered on the SNAT address).
- The Node address is the kubelet's: trusting it after tenant pods land would let a lost pin move to another
  worker's address. A partial node read must not shrink the deny list (layer 0 refuses the same way). The taint only
  ever came off, so a worker unfenced later kept taking tenant pods. CABPK can refresh the token shortly before the
  Node registers, so 15 minutes could end a few seconds early.

**Diagrams updated:** `c4-code.md` (C4.1 layer 1).

## 2026-11-07: The sandbox workers' VM firewall, the unsecured taint and their own egress address (SB2b)

**What changed**

- `pve.StartSandboxPass` / `Secure` / `Sweep` (`internal/cloud/pve/sandboxfw.go`): every VM of the provider's
  sandbox pool gets `firewall=1` on each NIC, VM options with both policies `DROP`, `ipfilter` and `macfilter` on and
  DHCP, NDP and RA off, and as its only rule a reference to one cluster security group per generation
  (`sbx<pool hash><content hash>`, with IP sets `-n` for the workers and `-o` for the internet minus every private and
  denied range). Every rule ACCEPTs: the control plane's API and konnectivity ports, DNS to the configured resolvers,
  VXLAN, Cilium health, MetalLB memberlist and the kubelet between workers, the kubelet from `kubeletSources`, the
  internet. `ipfilter-net0` holds the worker range until the Node joins, then the Node's address, pinned once, plus
  the MetalLB range; a NIC off the sandbox vnet gets an empty filter. A generation a worker uses that differs from the
  computed one fails the pass (`ErrSandboxDrift`) instead of being rewritten. New client calls for VM rules and options
  and for cluster IP sets.
- `syncjob.reconcileSandboxWorkers`, run with the capability detection each cycle: reads `config.sandbox` (`pool`,
  `nodes`, `loadBalancers`, `dns`, `kubeletSources`, `unsecuredMinAgeMinutes`) and `secret.sandboxKubeconfig`, takes the
  control plane address from that kubeconfig, fences each worker, then removes the `proxcloud.io/unsecured` taint
  from a worker read back in place, pinned to its Node's address and at least 15 minutes old. `kamajik8s` gains
  `SetNodeTaints` (merge patch guarded by resourceVersion) and `Server`.
- `deploy/sandbox-cluster/rbac.yaml`: ServiceAccount `proxcloud-system/proxcloud-firewall`, nodes get, list, patch.
- Installer: stage 10 adds role `ProxCloudSandboxGuard` (`Pool.Audit VM.Audit VM.Config.Network`) on
  `/pool/<sandbox pool>` for `proxcloud@pve` and its token, `NoAccess` there for `csi@pve`, and checks the token's
  effective rights on the pool. Stage 55 taints the node group, requires `sandbox.network.snat` (or `node`), writes the
  sandbox's SNAT address as an ifupdown2 dummy interface on the exit node (root ssh, group `pve`), turns the subnet's
  PVE SNAT off and deletes the rules PVE leaves, mints `generated/sandbox-firewall.kubeconfig`, and no longer waits for
  the agent-sandbox controller. Stage 60 writes `config.sandbox` and `secret.sandboxKubeconfig`.

**Why**

- Layer 0 sees routed packets only and leaves the services network open (tenants need it): a foothold on a worker
  still reaches the DBaaS load balancers and other tenants' control planes, the vnet gateway is a PVE node's own
  address, and nothing stopped address spoofing inside the vnet. Only the VM firewall sees every frame of a worker.
  The CAPMOX workers have no `SecureVM` and the chart no NIC firewall field, so the portal applies it after the
  clone, in the pass that already walks each provider.
- All-ACCEPT rules under DROP policies make the reconcile safe without ordering: PVE adds rules on top and has no
  atomic replace. Content-hashed generations make a change a switch rather than an edit.
- The Node address is the kubelet's to write; it is trusted once, while the taint keeps tenant code off, and pinned.
  The taint also covers the join token left on the worker's disk (CABPK TTL 15 minutes, SB0).
- The portal token inherited every privilege of `/pool` on the workers, guest agent and disk included, which a gVisor
  escape plus a portal bug could turn into a path back. An ACL on the pool replaces what `/pool` passes down there and
  nowhere else, so the role is narrowed exactly on the VMs that run tenant code.
- D14: abuse from tenant code must get the sandbox's address blocklisted, not the nodes' or the portal's. PVE SNATs
  to the exit node's own address and offers no other, so the rule lives on the exit node; with it missing the traffic
  leaves unrewritten and is dropped upstream rather than falling back to the node address.
- Kamaji's konnectivity server uses the default proxy strategy, so exec to a pod can go through another worker's agent:
  the kubelet port is open between workers, found on the lab when a second worker joined.

**Diagrams updated:** `c2-containers.md` (Sandbox cluster row), `c3-components.md` (Sync job, Networks and isolation
rows), `c4-code.md` (C4.1 layer 1 and diagram).
## 2026-11-07: tenant-side proxmox-csi settings and the PVE API forward removed (K8s-CSI, PR 4 of 4)

**What changed**

- `config.cluster.csiSharedStores` and `config.cluster.pveCsiUrl` are gone from the kamaji provider
  (`kamaji.ClusterDefaults.CSISharedStores`, `.PVECSIURL`). A new cluster gets proxmox-csi classes through the CSI gateway,
  or none; `sharedWorkers` keeps its tenant-side controller on the operator's token, now pointed at `pveApiUrl`.
- `pve.MintClusterTokens` mints the CAPMOX token alone (`ClusterTokenScope.CSIStorages`, `ClusterTokens.CSIID/CSISecret`
  removed). `RevokeClusterTokens` still deletes a `csi@pve!<id>` an older cluster holds.
- Admin create and update of a kamaji provider answer 400 to a `cluster` block that still sets either key
  (`externalservice.KamajiRemovedKeys`); an update that does not send the block passes. The admin form drops the keys on
  save, says so when the stored config has them, and no longer has the "PVE API for workers" field.
- Installer: the `pve-api` Service and EndpointSlice are no longer created (stage 50), and `services.pve_node_ips`,
  `services.network.pve_proxy_ip` and `services.kamaji.csi_shared_stores` are removed (preflight names them when a site
  still sets them). Stage 60 (`files/pve-forward.py`) deletes the forward on the services cluster when no
  `<id>-csi-config` there still holds a token and no managed cluster Application has StorageClasses without
  `proxmox.csi.gateway`, and otherwise names the clusters to move first; stage 60 stops sending the two keys.
- The "Changing a setting later" recipe in `docs/managed-k8s.md` drops the two keys from the `cluster` block it re-sends.
- The PR 3 migration action is unchanged, for sites with clusters made before the gateway.

**Why**

- PR 3's lab drill left no cluster on the tenant-side controller, so the setting only kept a way to hand tenants a token
  that opens every disk on a store. Removing it leaves the gateway as the one path, which is the owner's rule: no PVE
  credential in a tenant cluster.
- A stored key that silently stops doing anything would leave an operator wondering why new clusters have no classes;
  a 400 on write says so where the config is changed, and refusing only what the request sends keeps old rows editable.
- The forward existed only for tenant workers' proxmox-csi. Deleting it in stage 60, the stage `make upgrade` runs, is
  what reaches existing sites; checking the clusters' resource-set configs first keeps an upgrade from cutting off a
  cluster that has not been moved yet. The migration rewrites that config before it patches the Application, so the
  Application is checked too: a failed patch leaves the tenant-side controller on its old token, and removing the
  forward then would break its attach and delete until the migration is re-run.
- Every installer site's stored `cluster` block still holds the two keys, so the documented re-send recipe would now get
  the 400.

**Diagrams updated:** `c3-components.md` (Managed K8s create row). C1/C2 had no element or edge for the forward.

## 2026-11-07: Sandbox cluster setup and isolation drops for its network (SB2a)

**What changed**

- `deploy/sandbox-cluster/` (from the earlier implementation, EE): `node-setup` DaemonSet installing runsc with the
  systrap platform, RuntimeClass `gvisor`, the `CiliumNodeConfig` limiting socket LB to the host namespace, the Cilium
  guardrail with a TCP 25 deny, RBAC for `proxcloud-system/proxcloud` with no Secret access, agent-sandbox v1.0.9
  vendored with its licence, and `check-ipv4-only.sh`. node-setup refuses a host with IPv6. Left out: the NVIDIA
  branch and device plugin (SB9), the envd init container (SB5), EE's legacy egress policy (no old sandboxes here).
- Installer: `sandbox.*` keys; stage 10 creates pool `proxcloud-sandbox` and token `capmox@pve!sandbox` with a tenant
  cluster token's grants (token rows only) and checks it sees one ISO store; new stage 55 creates the vnet, applies
  an ArgoCD Application of the kamaji chart in `pc-sandbox` with no project label, checks NodeRestriction on its
  control plane, waits for the workers and applies the manifests, stopping on IPv6; stage 60 writes
  `network.sandboxCidrs` on the Proxmox provider. Filter `pc_sandbox_net_problem` checks the addresses and refuses
  an overlap with the tenant pool, the services, blocked and public networks; `pc_sandbox_guardrail_problem` refuses a
  services network the guardrail does not deny (a test keeps its list equal to `network-policy.yaml`). Stage 55 also
  refuses a VNI another vnet holds, and an `evpn`-isolation site without `sandbox.network.zone`. Stage 10 gives
  `capmox@pve` `PVEDatastoreAdmin` on a separate `sandbox.iso_storage`, which the ISO upload needs.
- `pve.ReconcileIsolation` takes the sandbox networks (`PVENetwork.SandboxCIDRs`, provider `network.sandboxCidrs`):
  the tenant pool's DROPs to the blocked networks also name them, and two new DROPs (`FORWARD`, `IN`) stop them
  reaching the pool, the management networks and `blockedCidrs`. An entry overlapping `cidrPool` or a blocked
  network (the management networks included) fails the reconcile before any write. A provider without the key gets exactly the old rule set.
- The rebrand gate's previous-backend check skips `deploy/sandbox-cluster/agent-sandbox/`.

**Why**

- SB0 measured both directions open: tenant VMs reached the sandbox workers on every port, Cilium's VXLAN included,
  and a worker's own namespace reached the NAS, the PVE API and the management host through the exit node's SNAT.
  The isolation rules already sit where routed traffic passes, so naming the sandbox subnet there is the smallest fix
  that closes both paths before SB2b's per-NIC firewall; measured on the lab, both directions now drop while the
  worker keeps its control plane and the internet.
- The cluster is the platform's, not a project's: no project label keeps it out of every portal sync and bill, and a
  token minted per installation keeps its CAPMOX credential off the services token. NodeRestriction and IPv4 are
  release gates (SB0), so stage 55 checks both instead of trusting the chart default and the vnet.
- The overlap checks exist because the DROPs are symmetric: a sandbox subnet over the management network would drop
  new connections from it to the nodes (PVE API, ssh), and one over the services network would cut tenants off their
  control planes. The services network stays open to the workers at layer 0 (tenants need it), so until SB2b only the
  guardrail keeps a sandbox pod off it, and that holds only if the network is in a range the guardrail denies.
- The vendored CRD contains the Kubernetes pod schema, which names another platform's in-tree volume plugin; the file
  must stay byte-identical to the release for its checksum, so the gate skips that directory instead.

**Diagrams updated:** `c2-containers.md` (Sandbox cluster row), `c3-components.md` (Networks and isolation row),
`c4-code.md` (C4.1 rule set and diagram).
## 2026-11-07: moving older clusters onto the CSI gateway (K8s-CSI, PR 3 of 4)

**What changed**

- New admin action `POST /api/v1/admin/service/{id}/k8s-clusters/{clusterId}/csi-gateway` (`?dryRun=true`,
  `?force=true`; ADMIN_SERVICE_MANAGE), `project.MigrateClusterCSI`. It moves one cluster made with the tenant-side
  proxmox-csi controller (`csiSharedStores`) onto the gateway: adopt, switch, clean up, and answers a report.
- Adopt: the portal reads the cluster's PersistentVolumes through its Kamaji admin kubeconfig (`kamaji.TenantKubeconfig`,
  `kamajik8s.ListPersistentVolumes`), those of every other cluster of the provider, and the services cluster's own
  (`kamaji.ServicesPersistentVolumes`; `deploy/mgmt-cluster/rbac.yaml` gains `list persistentvolumes`).
  `planCSIAdoption` takes a disk into the `csiVolume` registry as the cluster's only when its PV is the one PV naming it,
  PVE lists it on one of the cluster's stores, it is attached to the cluster's workers and to no other VM, and no row
  holds it; a disk no worker holds needs the operator to name its PV (`?adoptDetached=`). Refusals carry their reason
  and stop the migration (409 with the report) unless forced; unnamed, unheld, unattached `-pvc-` disks are listed as
  orphans and left alone. Disks are listed with the gateway token, VMs and their configs with the portal's. A cluster
  being deleted is refused; PVs made between the read and the revoke are reported as `late`.
- Switch: `kamaji.SwitchCSIGateway` writes `<id>-csi-gateway`, cuts `<id>-csi-config` to the `csi-proxmox` namespace and
  pins the provider's chart (0.1.7 or later) with the gateway values in one Application apply.
- Clean up: the tenant's `csi-proxmox/proxmox-csi-plugin` Secret is deleted and `csi@pve!<id>` revoked
  (`pve.Client.RevokeClusterCSIToken`; `RevokeClusterTokens` uses it too). Re-running is safe.
- The portal now calls tenant API servers (only for this action): C2 edge `api -> svc`.

**Why**

- A disk's name says nothing about its cluster (`vm-9999-pvc-<uid>`), and under the old flow any cluster could create a
  PV naming any disk. Adopting what a PV names would hand an attacker's crafted PV the victim's disk for good, so a disk
  named by two clusters, attached to someone else's VM, or any disk while some cluster cannot be read is the operator's
  call, not the migration's. Being attached to the cluster's own worker proves nothing on its own: the old token let a
  cluster attach another's disk to its worker.
- Nobody else naming a disk proves nothing either (review of PR 3): the portal sees only its own provider's clusters, so
  a services-cluster disk (Kamaji etcd, registry, DBaaS, same `vm-9999-pvc-` names on `local-zfs` on the lab), a disk a
  deleted cluster left behind, or another portal's disk is named by nobody, and is detached during a pod restart, a
  drain or a hibernate. A crafted PV migrated in that window would have kept gateway access to it after the token went.
  The services cluster's PVs now count as claims, and a detached disk is adopted only by name from the operator.
  Attachments are kept per VM, so a disk on a worker and on a foreign VM is refused.
- PVE shows a `vm-9999-…` disk only to a token with `Datastore.Allocate` on its store, since VM 9999 does not exist to
  hold `VM.Audit` on. The portal token's listing came back without the cluster's disks on the lab (every PV read "not on
  PVE"), hence the gateway token for the listing.
- One apply, because both controllers elect a leader on the same leases in the tenant (names depend on the driver only),
  so the services-side one takes over as the tenant one goes to 0; a two-step switch would leave a window with no
  controller or two writers. The resource set does not delete what it stops carrying, so the old token's Secret is
  deleted by name, and the token is revoked last so a failed switch leaves the old controller working.
- Lab run on a cluster made with `csiSharedStores` on chart 0.1.6: a PV naming another cluster's registry disk blocked
  the run (409); without it the disk was adopted, the switch landed in about 10 s, the pod's data and a later detach,
  attach and expand went through the gateway, the tenant's Secrets held no credential and the token was gone.

**Diagrams updated:** `c2-containers.md`, `c3-components.md`.

## 2026-11-07: proxmox-csi controller on the services cluster (K8s-CSI, PR 2 of 4)

**What changed**

- New clusters run proxmox-csi's controller on the services cluster, in `pc-<projectId>`, from a new chart template
  (`proxmox-kamaji-cluster` 0.1.7, `templates/csi-controller.yaml`): plugin, attacher, provisioner and resizer, each with
  the Kamaji admin kubeconfig (`admin.svc`) and the gateway config from `<id>-csi-gateway`. The tenant keeps the node
  plugin with the upstream controller at 0 replicas (the chart's top-level `replicaCount`; the `controller.replicaCount`
  the chart set before is no key of the upstream chart) and no config Secret; the resource set carries only the
  `csi-proxmox` namespace. No `csi@pve!<id>` token is minted.
- The create path mints the cluster's gateway credential (`Store.ControllerSecret`) when the portal has
  `PROXCLOUD_CSI_GATEWAY_URL` + `PROXCLOUD_CSI_GATEWAY_CA_FILE`, the compute provider holds the gateway token and the
  workers are the project's. Every storage profile is then a StorageClass; `csiSharedStores` no longer gates them. A
  provider whose pinned `chartVersion` is older than 0.1.7 (`kamaji.ChartHasGatewayCSI`) refuses the create with 503.
- `KamajiReleaseCluster` first revokes the credential and deletes every disk the registry still holds for the cluster
  (`Store.ReleaseCluster`, gateway token), and keeps the marker while PVE refuses one. The cluster's PVE tokens,
  addresses and dataset are released in the same pass even when that step fails.
- Registry-driven billing: `csigw.VolumeRows` (a VOLUME sync provider in the PVE sync) makes each `ready`/`deleting` row a
  VOLUME row of the project (`data.volume.k8sCluster`, the profile of its store, created at the disk's creation). It
  leaves a `creating` row alone for 15 minutes; after that the row turns `ready` if PVE lists its disk and goes if not.
  It closes rows whose disk is gone and takes size and volid from PVE. Disks match by name, so a file store's
  `<vmid>/` directory does not hide one, in the reconcile and in the release. The PVE volume
  sync keeps rows with `k8sCluster`; `VolumeRows` keeps all others. The portal answers 409 to a delete or action on one.
  `CSIDiskQuota` counts the registry, not those rows, so a disk is counted once; other quota checks now see the disks.
- Deployment: proxcloud chart `api.csiGateway` (port, Service with `externalTrafficPolicy: Local`, cert-manager CA and
  leaf certificate, NetworkPolicy admitting the port from `allowedCIDRs` only, the API's other ports unchanged); the
  listener reloads its certificate files. Installer stage 40 turns it on in split mode with the services nodes as the
  sources (`pc_ip_cidrs`). With three mgmt nodes it pins the API to the first one (`pc_node_with_address`), the node
  the gateway URL names.

**Why**

- The upstream chart cannot give its sidecars a kubeconfig, so the controller is our template; the in-cluster
  kubeconfig avoids the tenant API allowlist. Leases and capacity objects stay in the tenant: the scheduler needs the
  capacity objects (the CSIDriver says `storageCapacity`), and the lease names depend on the driver name only (seen on the
  lab), so PR 3 can switch a cluster with one sync.
- The release revokes the credential before it deletes, so nothing can create a disk behind it, and deletes from the
  registry, so no disk of a deleted cluster stays on the store unbilled and with no owner, as the tenant-side controller
  left any PVC still there at delete.
- Two VOLUME providers reconcile one (provider, project, type) set, so each keeps the other's rows instead of archiving
  them; a second row type or service id would have broken pricing rules and the project teardown.
- The gateway port must not be reachable from tenant networks: a Local Service keeps the caller's address, which the
  NetworkPolicy checks. On k3s a call to the node address meets kube-proxy's "loadbalancer IP" rule in `PREROUTING`
  before the svclb pod's hostPort rule, so klipper-lb's MASQUERADE never sees it. Checked on the lab with a test
  Service of the same shape: a services pod arrived as its node's address and was let in, the lab host was refused.
  Local answers only on a node with an API pod, hence the pin with three mgmt nodes; a mgmt VIP would let the API move.
  The CA keeps its key (`rotationPolicy: Never`) because every cluster's Secret holds it.
- The chart gate: a cluster on a chart without the services-side controller would get the tenant controller at 0
  replicas and no controller anywhere, so its PVCs would never bind. 0.1.7, because 0.1.6 is #108's NodeRestriction
  chart.
- A `creating` row whose disk exists is a create whose answer and confirm read were both lost (the name was unused
  before, so the disk is the row's). Left creating, the plugin's retry got 409 forever and the disk was in quota but
  not billed. Matching by exact volid missed file-store disks: the release closed the row and left the disk.
- A release that cannot run its CSI step (a disk in use, a provider without the gateway token) must not keep the
  cluster's PVE tokens and addresses: each part is safe to run again, so the sweep retries the whole release.
- New clusters only; existing ones keep their tenant controller until the migration (PR 3).

**Diagrams updated:** `c2-containers.md`, `c3-components.md`.
## 2026-11-07: Sandbox service ported, not reachable (SB1)

**What changed**

- `internal/cloud/sandbox` ported from the earlier implementation (EE): Sandbox CR rendering under gVisor, per-project
  namespace with ResourceQuota and the tenant label, per-sandbox Cilium egress policy, GPU capacity, exec, logs,
  timeouts, template builds, snapshots and the sync provider. Its unit tests came with it.
- `kamajik8s` gained what the sandbox code and the later `sandbox-proxy` call: `NewInCluster` (service account token
  re-read every minute), `ErrUnreachable` on transport failures, server-side apply / get / delete / merge patch of custom
  and core objects, ResourceQuota, lists across namespaces, nodes, and `exec.go` (WebSocket exec and streamed exec).
- Renames: every EE label and annotation (both of its domains) is now under `proxcloud.io/` (the tenant
  label is `proxcloud.io/tenant`, the one the SB0 guardrail selects), the GPU node label is
  `node-restriction.kubernetes.io/proxcloud-gpu`, the sandbox-init environment is `PROXCLOUD_*`, and the node runtime
  directory is `/opt/proxcloud-sandbox/bin`, mounted at `/.proxcloud`. SB2a's `node-setup` and SB5's `sandbox-init` must
  use the same names.
- The template registry is provider config (`config.sandbox.templateRegistry`), not a constant; empty means no image is
  treated as a private template.
- Provider kind `sandbox` (`externalservice.IsSandbox`, `SandboxConfig`), with the admin API refusing to create one or to
  turn a row into one. `cloud_sandbox.go` (create, delete, actions, live list) is in the project package with
  `Handler.sandboxFor` left nil and no caller. `cloud.TypeSandbox` exists; nothing syncs or bills it.

**Why**

- The plan ports the sandbox code in one piece before anything can reach it, so the later phases change behaviour, not
  vocabulary. The release gate is SB2b (node firewall) plus SB3 (routes, quota, billing, suspend gate): the SB0 spike
  showed a node-level foothold on the sandbox cluster reaches the PVE API, the NAS and the management host, so no tenant
  route and no provider row may exist before those land. Refusing the kind at the admin API keeps an operator from
  creating a row the later phases would then serve.
- A kubelet can set any label on its own Node except `node-restriction.kubernetes.io/` ones (with NodeRestriction on),
  so a placement label outside that prefix lets a compromised node draw other tenants' sandboxes to itself.
- A hard-coded registry host belongs to one operator; each site has its own.
- Checked on the SB0 lab cluster through the ported service from a build-tagged test, with the proxcloud service
  account's token: create to Ready, exec (stdout, stderr, stdin, exit code 7, a missing binary as a 400), pod logs,
  timeout change, the egress guardrail selecting the namespace the service labels (a private address dropped, the
  internet open), delete and the CR gone.

**Review fixes (same PR)**

- What: exec (`Exec` and `ExecStream`) treats a stream that ends without its status frame as an error (it returned
  exit 0); a refused exec upgrade reads the pod and answers 404 (pod gone) or 403 (no `pods/exec`) instead of
  `ErrUnreachable`; a pull secret source that cannot be read (RBAC, API error) is an error for a template build and a
  logged warning for a namespace ensure, not "no source". Comments now name ProxCloud's `pc-`/`pc-db-` namespaces and
  `pc-c-`/`pc-d-` ids and mark the cluster files SB2a adds.
- Why: v4 and v5 always send a status, Success included, so a missing one means konnectivity or the kubelet cut the
  stream and the command's outcome is unknown; an agent would have read a half-run command as success. A 403 or a
  deleted pod logged as an unreachable cluster sends the operator to the network. The lab service account cannot read
  secrets, so custom templates would have failed at image pull with nothing in the log.

**Diagrams updated:** `c3-components.md` (C3e).
## 2026-11-07: NodeRestriction on tenant API servers (PR #108)

**What changed**

- Chart `proxmox-kamaji-cluster` 0.1.6 sets `KamajiControlPlane.spec.admissionControllers` to Kamaji's default list plus
  `NodeRestriction`, from a new chart value `kamajiControlPlane.admissionControllers`. The portal's values do not carry the
  key, so every cluster on 0.1.6 gets the chart default. Existing clusters get it through the usual chart bump: the
  provider's `chartVersion` to 0.1.6, then Bump. `TestChartEnablesNodeRestriction` renders the portal's values through the
  chart and checks the plugin.
- Docs: `docs/managed-k8s.md` section 9 (what a node credential can do with and without it), section 3 (the admin Bump has
  now run on Proxmox); c3 row of the managed Kubernetes create.

**Why**

- Kamaji's default list has no `NodeRestriction`, and the chart did not set one, so every tenant cluster ran without it.
  The SB0 sandbox spike found it (PR #103) and patched its own cluster by hand. Measured on the lab, same cluster before and
  after the Bump: with 0.1.5 a worker's kubelet certificate patched another Node's labels, taints and cordon (200) and read a
  Secret of another namespace by creating a pod bound to itself that mounts it; with 0.1.6 both are refused (`is not allowed
  to modify node`, `can only create mirror pods`, `a mirror pod may not reference secrets`). Root on one worker no longer
  reaches the other nodes' scheduling or the cluster's Secrets through the node identity.
- Kubelets, Cilium, the proxmox-csi node plugin, MetalLB and workloads kept working after the roll, and a worker that joined
  afterwards registered with its group's label and taint, so turning it on for every cluster costs nothing on the workers.

Files: `deploy/charts/proxmox-kamaji-cluster/{Chart.yaml,values.yaml,templates/cluster.yaml}`,
`internal/cloud/kamaji/chart_admission_test.go`, `docs/managed-k8s.md`, `docs/architecture/c4/c3-components.md`.
## 2026-11-07: Delete-now frees a project stranded in deletion

**What changed**

- Delete-now (client `DELETE /project/{id}/now` and operator `DELETE /admin/project/{id}/now`, both
  `project.Service.DeleteNow`) accepts a `DELETE_IN_PROGRESS` project that has no `scheduledForDeletionAt`, and stamps the
  past schedule the deletion job selects by. The next sweep tears it down and removes the row. A `DELETE_IN_PROGRESS`
  project that has a schedule is the sweep's to retry and still answers 400 "Project is deleting".

**Why**

- Before PR #76 the operator's delete-now wrote `DELETE_IN_PROGRESS` alone and ran the teardown in a goroutine. A restart
  or a failed teardown left the row in that status with no schedule, which the sweep never selects. #76 stopped new rows
  getting there and named delete-now as the way out for old ones, but the status machine of PR #84 refuses delete-now from
  `DELETE_IN_PROGRESS`, so an old row had no way out at all. No current path writes that status without a schedule, so the
  exception covers only those rows. Delete-now rather than widening the sweep: the operator decides when a project
  stranded that long is torn down.

**Diagrams updated:** none (no new component or flow).
## 2026-11-07: PR #103 Sandbox cluster spike (SB0)

**What changed**

- New Planned element in C2b: the sandbox cluster, a platform-owned Kamaji cluster built from the cluster chart, with its
  workers in pool `proxcloud-sandbox` on their own vnet, running tenant code under gVisor behind a Cilium egress
  guardrail, with no CSI token and no NAS dataset. Docs only: no code, provider kind or route.
- `docs/managed-sandbox.md` section 0 records the lab spike that shaped it.

**Why**

- The sandbox port (phases SB0 to SB10) needs a cluster where an escape from gVisor reaches as little as possible. The
  spike ran one by hand on PVE 9.2 and measured what the diagram now commits to: exec works through konnectivity alone,
  so the services cluster needs no route into the sandbox vnet; the EE guardrail drops every private destination from a
  sandbox while the internet stays open.
- It also showed what is not there yet and why the element stays Planned: from the worker's own network namespace the PVE
  API, the NAS and the management host are reachable (the per-NIC VM firewall of SB2b is a release gate), Kamaji tenant
  control planes run without the `NodeRestriction` admission plugin (true of every managed cluster today), and Cilium's
  Gateway API does not work with the chart's kube-proxy setup, so TLS for sandbox hostnames ends in `sandbox-proxy`
  itself (D15: Go TLS listener, wildcard Secret issued by cert-manager on the services cluster).
- A review of the spike added three release gates the diagram's Planned element now carries. Without NodeRestriction a
  node credential can create a pod bound to itself that mounts any Secret and then read it, so one worker reads every
  tenant's secrets. Tenant VMs reach the sandbox workers on every port (one VRF holds both, the isolation rules do not
  name the sandbox CIDR, the worker NIC has no VM firewall), and forged Cilium VXLAN packets can skip sandbox ingress
  policy, so the sandbox CIDR must be dropped in both directions. Sandbox traffic reaches the NAS as a node address after
  the exit node's SNAT, which the exports accept. And the guardrail is IPv4 only, so the cluster must be single-stack.
- The bootstrap ISO's store is chosen by the cluster token's ACL (CAPMOX takes the first ISO store the token can see), so
  the sandbox token is granted a node-local store only. That is the trust boundary the C2b row names.

**Diagrams updated:** `c2-containers.md` (C2b).
## 2026-11-07: Acceptance pass: statuses and messages the consoles got wrong

**What changed**

- A Kubernetes cluster reads `READY` when Argo CD still says `Progressing` but the control plane is Ready with an endpoint,
  every node group runs its count of Ready nodes, the Application is Synced, and the one part of it not done is its
  ClusterResourceSet: every HelmChartProxy Ready, read off the management cluster. That
  holds for 30 minutes from the Application's creation; past it a ClusterResourceSet that never applies leaves the cluster
  `PROGRESSING`. `deploy/mgmt-cluster/rbac.yaml` gives the ProxCloud ServiceAccount read on HelmChartProxies and
  ClusterResourceSets; on an existing install re-run the services-cluster step to apply it. Without it the reads are refused
  and Argo's answer stands. The sync also reports the cluster's
  StorageClasses and whether it has ReadWriteMany, and the cluster page says when a claim will stay Pending.
- `LIST_VOLUME_TYPES` marks the provider's default storage profile; the server wizard preselects it.
- Activating a billing profile that has no currency gives it the base currency, an ACTIVE profile already stored without
  one gets it when read (and stored once), and a bill of such a profile reads in the base currency.
- Operator Delete now asks once more before it runs, answers a failure underneath as a 500 instead of 200, and its audit
  event records the flip to `DELETE_IN_PROGRESS`, which tells it from a scheduled deletion.
- The bill page's lines add up to the total: the subtotal is the items before adjustments, tax keeps its sign (credit-note
  bills), and a sub-cent remainder dropped by the cent-rounded total shows as Rounding. The bill DTO (`ToBillDto`) converts
  items and adjustments to the profile's currency at the rate its net and gross use, so every amount on a bill is in one
  currency.
- The refusal on a suspended project says who lifts it: paying the balance, or the operator for an operator's disable.
- The admin Kubernetes and database cluster lists answer a management-cluster error as a 502.
- Consoles: Delete now for operators, Test connection on Kubernetes and database providers, a first-project form for an
  organization member with no project, operator-activated profiles get deposits, and several labels (open bills, sub-cent
  amounts, deleted projects, inherited billing profiles, servers being deleted, SMB passwords, backups on delete).

**Why**

- Argo's health covers the whole Application, and its ClusterResourceSet fails until MetalLB's webhook answers, then waits
  out CAPI's retry backoff: on the lab the Application read `Progressing` 8 minutes past CAPI's `Available`, and the page
  said "Working on it" with every step done. Five lab creates turned Healthy 9.5 to 16 minutes after the Application was
  created, the last change each time the ClusterResourceSet. Argo stays the source for everything else: a broken chart or a
  ClusterResourceSet that never applies is not hidden behind READY for good. Argo 3 no longer keeps per-resource health in
  `app.status.resources`, so the parts are read from the management cluster.
- An operator can activate a profile before its owner saves billing details, which is where the currency came from. Its
  bill list then answered 500.
- An unreachable management cluster answered 200 with no clusters, and a Test connection built on that list would have
  said connected. Since #109 any untyped error answers 500; a cluster error answers 502, the cluster's fault.
- The bill net was converted to the profile's currency but the adjustments were not, so the subtotal (net minus
  adjustments) mixed currencies: EUR at 0.9, items $100 and an adjustment of -$10 printed a subtotal of 91.00, not 90.
- A new project's services are attached by the entry call that runs alongside the pages' first reads, so they read no
  location and showed "No region" until a reload. They read again once that call is done.

**Diagrams updated:** none (no new container, component or relation).
## 2026-11-07: Retype keeps one row; tenant views lose platform identities; named volumes

**What changed**

- A retype marks the volume row moving (`CloudResource.MovingSince`, at most 3 hours). A sync's delete-of-vanished keeps a
  moving row, and the follow then moves it onto the new volid (`ChangeExternalID`). `ChangeExternalID` fails when the row is
  gone instead of deleting the sync's row, and an upsert and a follow of one external id are serialised (advisory lock).
- An upsert that finds a row it did not read first carries the provider's merge (`Repo.Upsert`, `CacheMerger`).
- A move whose status read failed is waited out before the follow reads the disk's slot, and a disk the follow finds at its
  new place answers success.
- The retype's wait, follow and mark clear run on a context the request cannot cancel (bounded by the 3 hours of the mark).
- A retype of a row still marked moving answers 409.
- `providers.Reconcile` leaves a row alone whose `ExternalIDSince` is after its listing, and holds the pass's inserts for
  the next sync when such a row is in scope.
- The serial console proxy sends termproxy's opening line (`<user>:<ticket>`) itself. The action answers no user, and no ticket
  for a serial session. The events tab shows a digest for the task id and `platform` or `operator` for its user; the audit of
  a task run outside the portal keeps the type, node and status only.
- A repeated `ATTACH_PUBLIC_NETWORK` on a server whose public NIC is plugged answers 409.
- Volume create takes a name, kept on the row as `data.volume.label`; an unnamed standalone volume reads as its disk's file
  name instead of its path on the store.

**Why**

- The task poller syncs a project as soon as a move ends, before the retype's follow ran: the sync archived the row, the
  follow deleted the sync's new row and renamed a row that was gone, and the next sync added a third. The bill and history of
  the disk split in two (acceptance pass on the lab). Keeping the row while the portal moves it is what lets the existing
  follow do its work; a failing follow now leaves one row instead of none.
- The ingress cuts a request after 60 s while a disk copy goes on for minutes. On the cut request the wait returned at once,
  and the follow and mark clear failed with it: every sync then kept the old row and added one for the new volid, both
  billed until the mark ran out. A sync lists first and reads the rows last, so a follow landing in between had the row
  archived and the old volid inserted again. And one lost status read made a retype that had worked answer an error.
- The serial session's first line named the platform's PVE token to the tenant, and the events and audit carried the UPID
  and PVE user, which name it too, or an operator's account.
- Rewriting a plugged NIC gives it a new MAC, and the guest loses its public address until a reboot. The API answers other
  repeats ("already attached", "already on storage") with 409.
- PVE names disks itself and has no field for a name, so a tenant's name can only live on the row.
- A tenant whose request was cut may retype again while the copy runs. PVE refuses that retry ("can't lock file"), but its
  follow found the old volid in the slot and cleared the first move's mark, so the sync at the move's end archived the row
  and inserted a new one.

**Diagrams updated:** `c2-containers.md`.

## 2026-11-07: An untyped handler error answers 500

**What changed**

- `httpx.WriteError` writes a response for every non-nil error: a typed `*HTTPError` as before, anything else as a 500
  `internal.error`, with the error logged against the request id and kept out of the body. A nil error still writes
  nothing and returns false.
- The API sets `X-Request-Id` on every response (`exposeRequestID`), which is where `WriteError` reads the id.
- The package helpers that wrote their own 500 after a false return (org, project, sso, affiliate, order, project
  invites) now only call `WriteError`; accepting an invite still answers 404 when the project lookup fails untyped.

**Why**

- About 380 callers wrote an untyped error with `httpx.WriteError(w, err); return`, and about 320 used
  `if httpx.WriteError(w, err) { return }`. For a database or upstream error the first answered 200 with an empty body
  and the second went on to the success write: creating a tax rate with the database down answered
  `200 {"data":null}`, the flavor categories an empty list, and the UI showed success. The admin audit middleware read
  those 200s as successes and recorded the failed mutations.
- The order and project-invite routes sent the raw error text instead, which named the database user and address.
- Fixing it in `WriteError` covers every caller at once and keeps the change to the callers mechanical.

**Diagrams updated:** none; no container or component changed.

## 2026-11-07: Bootstrap ISOs off shared storage (SB-pre, PR #101)

**What changed**

- A Kubernetes cluster's CAPMOX token gets its ISO grant on the Proxmox provider's `isoStorage`, now `local` by default
  (was `nas-templates`). A create answers 409 when that store is shared, takes no ISO images, when the boot store also
  takes ISO images, or when `capmox@pve` lacks `PVEDatastoreAdmin` there (`pve.BootISOProblem`, `CheckBootISOGrant`).
- The kamaji sweep calls a new live-cluster hook, `OnClusterLive`; `KamajiPinBootISO` moves an older cluster's grant to
  the new store (`pve.PinClusterISO`) once no bootstrap ISO is left on the old one.
- Installer: `services.iso_storage` defaults to `local` and preflight refuses a shared one; stage 10 gives the services
  token `capmox@pve!capi` only its boot store and the ISO store, keeps `/storage` on the user, and takes the old
  `/storage` and NAS grants off the token. It then asks PVE (`/access/permissions`) which `iso` stores the token sees and
  fails unless that is the ISO store alone. `make upgrade` now runs stage 10; `make identity-check` runs it with
  `--check` (logins and reads run there too) to show what it would change.
- The move gives an old store back when a node started booting from it during the move, and waits. The user's ISO grant
  is read off the ACL list the way PVE resolves it (`effectiveRoles`: groups, propagate, a deeper entry replacing its
  parents) instead of one exact row.
- Test connection warns about each network store outside `blockedCidrs` and the nodes' management networks (NFS, CIFS,
  GlusterFS, iSCSI portals, an external Ceph's monitors; `StorageDef` carries `server`, `monhost`, `portal`), and when
  the provider's `isoStorage` is unfit (`bootISOWarnings`). `externalservice.PVEBootISOStorage` replaces the project
  package's own reading of `isoStorage`.

**Why**

- A bootstrap ISO holds the node's kubeadm join token, renewed until the node joins. On the NAS any VM whose traffic
  reaches the export can read it and join a node of its own to the cluster, the services cluster included. Drilled on the
  lab with CAPMOX's own go-proxmox calls: a VM with a public NIC mounted `nas-templates` and read the token; with the new
  grant the same ISO went to `local` and nothing reached the NAS.
- CAPMOX v0.9.2 names no store, so go-proxmox takes the first `iso` store in the token's view, and PVE lists stores in
  Perl hash order: the old services token saw three and the pick changed between calls. Pinning one store by ACL is the
  only lever, hence one iso store per token and the check on the boot store.
- Moving an old grant while a node is still booting from the NAS would leave that ISO there for good: CAPMOX deletes it
  only on stores its token still sees. The sweep waits instead, and looks again after the revoke, since a node can start
  booting between the first look and the revoke.
- The services cluster's join ISOs are the most valuable ones, and `make upgrade` did not run stage 10, so an existing
  site kept a services token that saw every iso store. The check makes the rule hold on the site rather than only in the
  ACL list the installer meant to write. Dry-run on the lab: stage 10 would grant `/storage/local-zfs` and
  `/storage/local` to the token and take `/storage` and `/storage/nas-templates` off it; today the token sees `cephfs`,
  `nas-templates` and `local`.
- A false 409 on a group grant, a missed Ceph or iSCSI store, and a shared `isoStorage` that held existing clusters in
  place silently were all review findings on this PR.
- Server seed CDs stay on shared storage, since PVE does not migrate a VM with a node-local CD. They are as safe as the
  store's reach, and a vnet reaches a NAS under a node's address through its NAT, so a node-only export does not stop
  it: the isolation rules do, and the warning says when a store is outside them.

**Diagrams updated:** `c3-components.md`, `c4-code.md`.

## 2026-11-07: CSI gateway and volume registry (K8s-CSI, PR 1 of 4)

**What changed**

- `internal/cloud/csigw`: a listener in `cmd/api` (`PROXCLOUD_CSI_GATEWAY_ADDR`, TLS only, off by default) that answers
  proxmox-csi-plugin v0.22.0's controller as if it were PVE. Thirteen calls are allowed, each with its own check; every
  other path, nextid, VM create, replication, migrate and `/access/ticket` included, answers 403 and writes an audit
  event (`CSI_GATEWAY` / `DENY`).
- Per-cluster credential `csi@proxcloud!<clusterId>` (`csiGatewayCredential`, SHA-256 only), minted and revoked by
  `Store.MintCredential` / `RevokeCredential` for PR 2 to call at cluster create and release.
- `csiVolume`: the gateway's record of the disks it created, keyed provider/store/name and written before PVE is asked,
  so a name is taken once across clusters. `csiTask`: the UPIDs it answered, the only ones a cluster may poll; a finished
  delete, copy or resize lands on its row once.
- `pve.Client.Raw`; `ExternalService.PVECSIGatewayConfig` (`config.csiGatewayTokenId`, `secret.csiGatewaySecret`);
  `project.Handler.CSIDiskQuota` counts registry volumes against `diskGb`.
- Installer: stage 10 creates `csi@pve!gateway` (role `ProxCloudCSI` on `/pool` and `/storage`) in split mode, stage 60
  puts it on the compute provider.

**Why**

- The tenant-side CSI token opens every disk on a store, since PVE checks only `Datastore.Allocate` there, and moving the
  controller out of the tenant is not enough: a tenant still writes the PV that names the disk. A gateway that checks
  ownership is the boundary, and only it holds a PVE token.
- Ownership is a registry row, not a tag or a name prefix, because the plugin's disks carry neither, and nothing a tenant
  controls (volumeHandle, NodeID, providerID, StorageClass) is taken as proof.
- The worker check reads `/cluster/resources` with the portal token: a token without `Pool.Audit` gets no `pool` field
  there (PVE 9.2, lab), and `ProxCloudCSI` stays as it is. The portal token also cannot see `proxcloud-system`.
- Its own port, not `:8080` (public) or `:8081` (`/debug/*` has no auth).

**Review fixes in the same PR**

- The worker list is read on every VM call, not cached for 10 s as the design had it, and the `pc-k8s-<id>` tag is
  checked again on the VM config the gateway reads. Why: PVE gives a freed vmid to the next VM at once, so a cached list
  let a cluster read the config of, attach to and revert another project's VM under its old worker's id for up to 10 s.
  The store list stays cached: stores never change hands.
- Quota check and registry write run under a Postgres advisory lock per project (`Registry.Locked`). Why: concurrent
  creates, copies or resizes each passed on the same usage.
- A resize writes the new size to its row when PVE accepts the call (set back if PVE refuses). Why: the row only moved
  when the cluster polled the task, so a cluster that skipped the poll could grow each disk to the whole quota, and
  billing from the registry would undercount.
- A create or copy whose name PVE already holds without a row is refused (409). A create whose answer is lost asks PVE
  whether the disk exists: yes, the row turns ready; no, it goes; no answer, it stays `creating`. A copy whose answer is
  lost keeps its `creating` row. Why: an older disk with no row (another cluster's, before the gateway) must not be taken
  over or blocked for the migration's adopt step, and a disk must never be left without a row.
- The listener has read, write and idle timeouts, and an unknown cluster id costs the same hash compare as a known one.

**Diagrams updated:** `c2-containers.md`, `c3-components.md`.

## 2026-11-07: Load balancers (HAProxy appliance)

**What changed**

- `LOAD_BALANCER` on a Proxmox provider: customer page, API, sync pass, delete. Each is a VM in the project's pool running
  HAProxy, on a project network and a public network, configured through the guest agent. Members are servers of the
  project; the platform resolves their addresses.
- The VM, disk and address are ordinary SERVER, VOLUME and FLOATING_IP rows; a tenant's direct action on them answers 409.
- `pveCreateServer` split into `pveBuildServer` and its handler, so an appliance goes through the same quota, placement
  and firewall path.
- `PublicIP.BeforeRow`, because the orphan check took an address allocated during a build for a reused vmid.

**Why**

- PVE has no load balancer service, and a tenant needs one address in front of several servers. A VM rather than something
  shared keeps one tenant's traffic and config out of another's way, and lets the existing metering bill it with no new
  price type, which is what removing the inert `load_balancer` type left room for.
- The seed CD route was tried and dropped: PVE wants `Datastore.Allocate` to delete an ISO, so each balancer left a file on
  the store. PVE's own cloud-init drive plus the guest agent leaves nothing and needs no extra privilege.
- Names stay out of the config, and members are named by server, because both are tenant input that ends up in a file root
  reads on a VM with a public address.
- A failed server create now destroys its VM and frees addresses only once PVE confirms the VM is gone (`VMLeftError`
  otherwise), on the shared path, because the read-back after boot could fail with the VM running and hand its address to the
  next tenant.
- One check on the target VM's tag where a request names a VM, because the guard looked only at the row acted on: a tenant's
  disk moved onto the appliance could not be detached and was destroyed with the balancer.
- HAProxy drops to user `haproxy` in a chroot: this config replaces Debian's, whose lines did that, and it faces the public
  network.

**Diagrams updated:** `c2-containers.md`, `c4-code.md`.
## 2026-11-07: Protection survives a rollback; seed CD removal is retried

**What changed**

- A rollback or a restore keeps the VM's protection tags as they were. A live replication job outranks the tag: the sync
  and a read derive the tag again from it, and disable and delete act on a job or an HA resource with no tag.
- Remove protection flags the job first, then drops the tag, then removes the rule and HA.
- An HA-managed VM must be down before a resize, rebuild or holder move goes on: up to 3 minutes, then an error, and HA is
  asked to keep the VM up. A resize reports a failed start, and an HA start that has not landed after a minute.
- A keep-volumes delete waits for pvesr before moving disks to the holder.
- `DeleteSeedVolume` returns an error, overwrites after any failed delete, and tries a shared store from other nodes. A CD
  still holding content leaves a row with only its file name, which the sync retries. A failed attach cleans up the same way.
- Set password with no username goes to the seed's login user. Passwords over 72 bytes answer 400.

**Why**

- PVE puts tags back with the config on a rollback (lab, PVE 9.2), and the job and HA carry on. Billing stopped for live
  protection, disable did nothing, and an older snapshot could revive a closed bill line. Keeping the tags at the rollback covers
  all of these in one place, and deriving them from the job covers a rollback run outside the portal.
- A stop under HA can take the full 120 s of a shutdown, and the old one-minute wait then let the config write and the start go
  to a running VM. The start was lost, and the resize still reported success.
- A reconcile between HA's removal and the job's would add HA back with nothing left to heal it. With the job flagged first it
  no longer counts the job, and with the tag gone it leaves a shared VM alone.
- A CD whose removal failed lost its row and so any retry, with the tenant's user-data still on the store. The kept row has no
  server id because PVE reuses vmids: the next server must not read this seed, nor hold up the retry by having a row.
- bcrypt takes 72 bytes. Longer failed a create after its clone, and a Set password left the old hash for the next rebuild.

**Diagrams updated:** none, no container or component changed.
## 2026-11-07: Tenant clusters get no PVE token by default

**What changed**

- A cluster on a project network gets no proxmox-csi, no CSI token and no tenant-side PVE credential, unless the provider sets
  `config.cluster.csiSharedStores`. Chart 0.1.5 leaves the plugin out when no StorageClass is offered and makes
  `shared-file` the default class.
- The minted CAPMOX token gets `/sdn/zones/localnetwork`, worker node groups count against the project's quota, and a
  revoke treats a missing token as revoked: only PVE's 500 naming it, not any error that says "no such".
- A provider's numeric config keys accept a string of digits. A node group may keep a retired flavor it already runs, and
  moves off one; its quota is counted at the size it really holds, root disk included.

**Why**

- First real run of the minted path. PVE checks only `Datastore.Allocate` on the store to attach or delete a disk, not the VM
  that owns it, and proxmox-csi needs that privilege. A tenant cluster-admin read the token from its own cluster, attached
  another project's PVC to its own worker and read the files. The token's ACLs cannot prevent this, and moving the plugin
  to the management side would not either, since a tenant can create a `PersistentVolume` naming any disk. Off by
  default, opt-in with the cost stated, is the same call as `snatSources`. A store per project would be the real fix.
- Every clone failed without the node bridge grant, because PVE checks the template's NICs. A revoke failed for ever on a
  token already gone, because PVE answers that with a 500. Workers bypassed every quota but GPU.
- Ansible's `{{ x | int }}` reaches the API as a string, which read as 0: a site's `rwx_gib` and `worker_mtu` did nothing
  and a VLAN zone from the installer had no tags. A revoke that took a DNS error for "no such token" would have left the token
  live with its marker gone, and nothing retries after that.
- The setting decides what a cluster is created with. It does not reach an existing cluster's token, so
  `docs/managed-k8s.md` gives the procedure to take one back rather than a new action that only an operator should run.

**Diagrams updated:** `c3-components.md`.
## 2026-11-07: A resume is retried, and brings back only what a suspension paused

**What changed**

- The suspension walk writes `pausedBySuspension` on a VM's `cloudResource` row before pausing it. The resume leg touches marked
  rows only, and clears a mark once it reads the VM as running or stopped on its node. `ResolveVM` reports a failed node read in
  `ClusterResource.StatusErr`, and the walk leaves such a VM to the next pass.
- `CloudSuspender` gains `FinishResumes`. Each `autoSuspensionJob` pass resumes marked VMs in `ENABLED` projects, and enables first a
  project left `DISABLED` under a profile that pays, unless an operator disabled it.
- `ActivationService.Unsuspend`, `SuspensionJob.resume` and the billing callbacks run the walk on `context.WithoutCancel`.
- `finishSuspensions` reads each profile again just before walking it.
- A project move onto what cannot pay skips a provider that has been removed, and answers 503 when a provider that is there cannot
  be read, in place of a 500.
- `DELETE /admin/billing-profile/{id}` uses the client path's `ProjectPaidBy` rule, so a profile paying through its organization is
  refused. `PUT /admin/project/{id}` applies `holdForProfile` to an `ENABLED` or `DISABLED` project whose paying profile changes.
- CI's integration job also runs `cmd/api`, where the walk's tests live, since `billingCloudSuspender` is in `main`.

**Why**

- A failed resume stayed paused for good on an `ACTIVE` profile and an `ENABLED` project, as passes only looked at `SUSPENDED`
  profiles. A pass cannot resume every paused VM instead: PVE has no record of why a VM is paused, the old resume leg already undid
  pauses it did not make, and the walk recorded nothing per VM. The mark goes before the pause because the other order can crash
  into a paused VM nothing knows about, and a mark on a running VM costs one read.
- The mark is on the row, not on the VM's PVE config: a config write is one more PVE call that can fail on a VM being paused, and
  the row already survives a sync because `toDbRecord` writes named fields only. A VM paused by a build older than the mark has
  none, and comes back through **Start**.
- `/cluster/resources` reads running for a paused VM, so one failed node read cleared a mark with no resume and no log line, and on
  a suspend marked a VM someone else had paused. A flag on the result, because the other callers of `ResolveVM` want the fallback.
- A customer who had just paid could be paused again by a pass working from an old list.
- Removing a provider left every move of its projects failing with `internal.error`, and a failed read must stay a refusal: the live
  check is there because a row can lag a start.
- Operators could delete the profile an organization's projects pay through, and move a project onto a profile that cannot pay,
  leaving it `ENABLED` and unrated.

**Diagrams updated:** none.

## 2026-11-07: Database ids take a neutral prefix

**What changed**

- New managed databases get ids of the form `pc-d-<8hex>`. Existing databases keep the id they have.
- A restore's source id may carry either prefix.

**Why**

- The earlier prefix came from the old brand, and it was the last one in use. An id names every object of a database
  (Application, release, engine CR, secrets, Service, its folder in the backup store), so it cannot change on a live database.
  The one place that read the prefix was that restore check, which would have refused a source with the new one.

**Diagrams updated:** none; no container or relationship changed.

## 2026-11-07: One bill per profile and cycle, the month's last hour billed once

**What changed**

- `ChargeBillingResources` runs get-or-create, rating and save in one transaction under the profile lock, the same
  advisory lock payments already hold. The `lockedAt` lease and its retry are gone.
- The admin bill update and delete, and the whole of the bill send, now hold that lock too. The send reads the bill
  again inside it.
- A minute or hour pass on the 1st at 00:00 bills the month that just ended, using that month's metrics. It never opens
  a bill. `monthlyBill` moved from 00:00 to 00:30 so those passes run first.
- The send worked out "last month" from the 1st. On the 31st after a shorter month it landed in the current month.
- `docs/billing.md` has queries to find duplicate bills on an installed site and to delete the empty ones.

**Why**

- `hourlyCharge` and `monthlyCharge` share a cron spec and have different ShedLock names, so they ran at once and both
  inserted a bill. An item first seen on the second bill starts at the cycle start and charges the month again, and both
  bills get invoiced: two OPEN bills in 113 of 200 passes in the review's repro, over-billed in 27 of 40 rounds.
- The lease did not hold, because a retry added a minute to its own clock instead of waiting. A second claimant got the
  same bill and its save replaced the first one's charge. Under a real lock a lease would only refuse the second pass.
- One lock rather than a charge-only one: the send and the admin update replace the same document. A test with a
  separate key lost a closing charge to a racing send within 7 rounds.
- The 00:00 charge opened the new bill with nothing elapsed, so the month's last hour was never billed: a VM running
  21:00 to 01:00 paid for 3 hours of 4. On the old wall clock the same pass would have read the new month's metrics and
  overwritten the month's traffic total.
- Issuing the running bill on the 31st let the next charge open another one that re-billed the month.

**Diagrams updated:** `c4-code.md` (charge rows). No container or relationship changed.

## 2026-11-07: Restore checks volumes and quota; a server delete drops backups last

**What changed**

- Restore compares volumes, not slots. A volume is held by an archive when the archive names its volid and its VOLUME row
  is older than the backup; anything else is refused by name. The boot disk always counts.
- A sync leaves a server's VOLUME rows alone while a restore task runs, and a disk with no row before the restore dates from
  that backup (`dataSince`). A retype keeps the row and records when it took the new volid (`externalIdSince`).
- A failed restore: the rows of the destroyed disks are archived, and the next restore is allowed through and counted by
  what the rows say.
- Restore checks vCPUs, RAM, disk and GPUs against the project's quota, on what it would produce, before stopping the
  server and again after. A snapshot rollback checks vCPUs, RAM and GPUs the same way.
- A server delete removes its backups after protection is off, the volumes have moved and a hard stop has worked, just
  before the destroy, and refuses a VM holding a lock or under a running restore. It lists the backups before its first
  change, so a store that cannot answer refuses with the server untouched.

**Why**

- A disk added after another left landed on the same slot, and PVE gives it the same name as well, so a forced restore
  destroyed it while it was in no archive. The row is the one thing the portal holds that tells the two apart, so it has to
  outlive a restore: PVE cuts the VM's config to `{memory: 128}` meanwhile, and a sync in that window archived every row of
  the server, which also restarted their bills.
- A retype can land on the name of a disk that left, on a row older than the backup, so the row's age said nothing there.
- Resize down, spend the freed quota, restore: the project ended over its limit, and a disk now on another server was
  recreated uncounted. A rollback restores cores and memory too, so it was the same bypass.
- After a failed restore the VM kept billing for destroyed disks, a retry counted them twice, and the NIC check refused the
  retry because the cut-down config has no NICs. A restore killed halfway also leaves its volumes on the storage in no
  config, and the next restore fails on them. The token cannot delete a volume from the storage, but it can list one as a
  detached disk and drop that entry, which frees it, so a failed restore and a retry both do that.
- A refusal from protection, from moving a volume or from a failed stop left the server alive with its backups gone. The
  delete cannot simply go last: PVE lets the token delete an archive only while its VM exists.
- A running restore holds no lock on the config it has cut down, so the lock check alone let a delete remove both archives
  under it. A store that was down refused only after the stop and the volume moves, leaving the server stopped and its data
  on the holder.

## 2026-11-07: Image import goes through the portal

**What changed**

- The portal fetches an image URL and streams it to PVE's upload API. `download-url` is gone from the portal, and
  `ProxCloudPlatform` no longer carries `Sys.AccessNetwork`.
- `pkg/safeurl.Blocked` blocks everything not globally routable. `Check` became `Open`, which hands back the fetch.
- Project images count against `diskGb`. One import at a time per project, and one upload at a time per provider. Room is
  checked on the import store, on the profile's boot storage for the disk, and on the API node's root disk, where pveproxy
  spools an upload. A URL with no stated size is refused unless the operator sets `importUnknownSizeMaxGiB`.
- An import waits for its upload task, whatever happens to its call, before releasing the lock and removing the file.
- A source that sends nothing for a minute, or averages under 2 MiB/s past a two-minute grace, ends the import (409).

**Why**

- The portal checked a URL and then a node fetched it again, following redirects PVE gives no way to turn off. A server
  answering the portal with an image and the node with a 302 reached the node's network, and the portal could not see
  it. With the portal as the only client, the check at connect time covers every address the fetch uses, and the node
  has nothing to fetch.
- The 200 GiB cap held per image with nothing on the total or on concurrency, so a tenant could fill the store.
- The room check on the API node is per upload, so uploads of several projects could each pass it and together fill the
  root disk. A failed poll of the upload task returned with the file still landing, the lock released and nothing to remove it.
- The block list let through ranges that reach no one on the internet but can reach something inside: benchmarking,
  IETF protocol, reserved, NAT64 and 6to4 among them. The SSO fetcher uses the same function, so it gains this too.
- A slow URL held the provider-wide upload lock for as long as the import timeout, an hour, so one tenant could keep other
  projects' imports at 409 at almost no cost. A stall limit alone would not stop a drip, so the source also has to keep up a
  floor: holding the lock now takes really sending the bytes.

**Diagrams updated:** `c3-components.md` (the image import row).
## 2026-11-07: A charge with an unknown outcome blocks new charges until resolved

**What changed**

- The in-flight guard covers every `PENDING` charge of a bill, not only those younger than 24 hours.
- `GET /admin/collect-transactions/{id}/sync` re-checks a charge at the gateway, and
  `POST /admin/collect-transactions/{id}/close` ends one the gateway says did not succeed, or one with no gateway
  reference. Both are in the operator console.
- A gateway answer of cancelled now ends a charge; it used to stay `PENDING`.

**Why**

- Past the scanner's 24 hours the guard let a new charge through while the old one's outcome was still unknown, so a long
  gateway outage could charge a customer twice. Lifting the age limit needed a way to resolve the charge, or the bill
  would stay blocked for good: a lost confirm on a declined card stayed `PENDING` with nothing able to end it.
- A blocked bill still counts for dunning and auto-suspension. Leaving it out of `DueBills` would cover the due-date type
  only (the balance type reads the balance), and a customer whose card needs an action nobody completes would never be
  suspended. A re-check that settles the charge resumes the profile, as any collect does.
## 2026-11-07: Leftover holders stop billing; a late lock release frees nothing

**What changed**

- A VM with no name, no tags, no disk and a config holding only what PVE writes for an empty create is no server: the sync
  neither lists nor bills it. `Service.RemoveBlankVMs` removes it on each full sync, under the holder lock, once it is past 15
  minutes old. A VM that matches on its row but not its config is listed as ever; one whose config cannot be read keeps its row.
- `ShedLock.Unlock` writes only if the row still carries the caller's `lockedBy` and `lockedAt`.
- `RemoveBlankVMs` bounds its pass, as the sync goes through projects one at a time and a hung destroy would hold them all.
  `dropLostCreate` uses the same `blankConfig` test, so a nameless VM holding a NIC or a description stays.

**Why**

- A holder VM is created empty and configured once it is in the pool. The cleanup ran only in the request, so PVE staying away,
  a restart or a failed destroy left a VM billed and shown as a server. The check reads the config, not only the row, because a
  row with no name can be a customer's VM with a data disk. The grace outlives the holder lock's 10 minute ceiling.
- A holder past its lock's ceiling released whatever row was there, freeing the next owner's lock. A later owner's lock now
  survives this. Two holders can still overlap once the first runs past the ceiling: this fixes the release, not the ceiling.

**Diagrams updated:** none.
## 2026-11-07: One bad provider no longer blocks a project; every object-store row is checked

**What changed**

- `POST /admin/service` and the updates check every `ceph-s3` and `rustfs` row whatever its status (`CheckRustFS` is new).
  Turning a row off stays unchecked.
- `POST /admin/service/{id}/object-store/test` tests a stored row with its stored key, as **Test connection** on the
  provider's Connection tab. The Add dialog's button now works for `rustfs` too.
- Bootstrap gives each provider its own bounded attempt. A failure goes in `project.provisionFailures`, retried on the next
  open at most every 5 minutes, and in `externalService.provisionError`, shown on the provider's page. Opening a project
  fails only when it ends up on no provider.

**Why**

- Only a `PUBLIC` `ceph-s3` row was proved, yet bootstrap provisions every non-disabled object store and stopped at the first
  error, so a `PRIVATE` or `rustfs` typo blocked every new project. A row can also go bad after it is stored, so the check
  alone would not be enough. Without the record on the project, a project that had any provider would never get the failed
  one, since a re-entry skips provisioning.

**Then, on the same PR, what a recheck found**

- A failed attempt is recorded on the provider under its own short context. The attempt's had often just expired, so a hung
  store, the likeliest failure, never reached the provider's page.
- A project on no provider gets an error on every open, not only the first. Inside the retry window it looked fine and was not.
- The admin API's project answer carries `provision_failures`, because a partial provision answers 200.
- Stage 60 checks a `rustfs` store from the API and shows the reason, as it does for Ceph. The task that registers it is
  `no_log`, so a refused row used to abort the install with the reason hidden.

**Diagrams updated:** none; no container or relationship changed.

## 2026-11-07: One table holds a project's status changes; a member cannot lift a suspension

**What changed**

- `internal/platform/project/status.go` holds every allowed status change. The client service, the client handler, the admin
  routes and the billing suspender all ask it, and writes are conditional on the status they read. `DELETED` and
  `DELETE_IN_PROGRESS` refuse every action.
- Cancelling a deletion needs `SCHEDULED_FOR_DELETION` and restores the status kept on the doc in `statusBeforeDeletion`.
- `POST /project/{id}/billing/{bp}` and `POST /project/{id}/init` no longer enable a project. A billing resume leaves a project
  an operator disabled (`disabledByOperator`).

**Why**

- Enabling was reachable from each path on its own terms: a cancel with nothing scheduled, a profile change, opening a project
  with no service yet. Each could lift a billing or operator suspension, and each could overwrite `DELETE_IN_PROGRESS`, which
  drops the project out of the sweep mid-teardown. A rule per handler is what let them differ.
- The write is conditional because the deletion sweep moves a project between a handler's read and its write.

**Then, on the same PR, what a recheck found**

- `Repo.Save` no longer writes `status`, `scheduledForDeletionAt`, `statusBeforeDeletion` or `disabledByOperator`. Only the
  conditional writes do: `SaveIfStatus`, `EnableIfDisabled` and `ApplyBillingSuspension`. Admin project update and quota write
  their own fields, where they replaced the whole doc.
- `pgdoc.SetFieldsOne` and `DeleteOne` lock the row they select. Without it a write queued behind another rechecked only the id,
  so two conditional writes could both win. The test that races a suspension against a member's schedule found this.
- A `DISABLED` project refuses what adds usage (`errProjectDisabled`, 409): every create, an upload, new or rotated S3 keys, and
  every action outside `allowedWhileDisabled`. Reads, stop and delete stay open.
- Moving a project to a `NEW` profile, by profile or by organization, is refused while something would keep running.
- A billing profile's suspend, resume and activation apply only to the projects it pays for (`AllByBillingProfile`, `PaidBy`).
- Opening a project lifts a `DISABLED` that nothing holds any more: not an operator's, and billing allows it. Activation lifts a
  provisioned project as well.

**Why**

- `Save` ran from a read that could be minutes old: a bootstrap waiting on a provider put back the status from before a member's
  delete-now, and the project dropped out of the sweep. Conditional writes on the few status paths do not help while a generic
  write can overwrite them, so the generic write must not hold the field at all.
- Metering, the sync and rating cover only `ENABLED` projects, so anything created or started in a `DISABLED` one ran free.
- Refusing the profile move, rather than pausing through the suspender, because the move is the tenant's own action and
  activation has no leg that resumes VMs. A refusal has no partial failure and tells them what to do.

**Then, on the same PR, what a second recheck found**

- `ActivationService.Suspend` and `SuspensionJob.suspend` save `SUSPENDED` before the cloud walk. `SuspensionJob.ExecuteDunning`
  starts by walking every `SUSPENDED` profile again (`finishSuspensions`), before the check that dunning is on. The walk skips what
  is already down. When a walk ends and the profile reads `ACTIVE`, the resume leg runs once more.
- A process already `SUSPENDED` on an `ACTIVE` profile no longer stops dunning: it is a suspend whose profile save failed, and the
  next pass carries on from it.
- The client's `DELETE /billing-profile/{id}` refuses a `SUSPENDED` profile, a negative balance, or one still paying for a project.
- `holdForProfile` takes "no profile" as well as a `NEW` one, covers every row type billing rates (`billableRows`) and a `DISABLED`
  project too, and does nothing when billing is unconfigured.
- `KeepServerVolumes`, `RebuildServer`, `ResizeServer` and `RestoreServerBackup` test `PoweredOn` before changing a VM: a paused VM
  reads `paused`, so they skipped the stop. `START` on a paused VM resumes it.

**Why**

- With the walk first, a member opening a project during it saw `DISABLED` on an `ACTIVE` profile and lifted it, and the suspension
  was lost for good. Saving first closes that, and leaves the failure cases to choose between. `ACTIVE` with projects down and
  nothing retrying is the one to rule out, so a failed save touches nothing and a failed walk stands as `SUSPENDED`.
- The pass is level-triggered rather than a retry marker. A marker only knows what a walk reported, and it would miss a crash, a
  VM whose row appeared after the walk, and a lift racing the save. The price: an operator's enable under a suspended profile lasts
  until the next pass, because nothing tells it from a missed project.
- A suspended project could get out by deleting the profile, since `updateOrganization` skips its check when there is none. An org
  with no profile was skipped too, and left a running project `ENABLED` and unrated.
- Billing unconfigured leaves every profile `NEW`, so the hold refused or disabled every move on a portal that bills nothing.
- Buckets and volumes were not in the check, so a tenant could park storage on a `NEW` profile, `DISABLED` and unrated, with its S3
  keys still working against the store.
## 2026-11-07: Cluster ids take a neutral prefix

**What changed**

- New Managed Kubernetes clusters get ids of the form `pc-c-<8hex>`. Existing clusters keep the id they have.

**Why**

- The earlier prefix came from the old brand. An id names every object of a cluster (Application, release, CAPI Cluster,
  TenantControlPlane, secrets, PVE tokens, share dataset, DNS), so it cannot change on a live cluster, and nothing read the
  prefix, so both forms can live side by side.

**Diagrams updated:** none; no container or relationship changed.

## 2026-11-07: Project teardown removes its services-cluster namespaces

**What changed**

- Teardown deletes the project's `pc-<id>` and `pc-db-<id>` namespaces once nothing is pending and no Application of the
  project is left, also when the project never had a cluster or a database.

**Why**

- Bootstrap makes both namespaces up front, and the orphan sweep only collects one on a pass that reaped something, so a
  project that never used them left two empty namespaces behind for good.

**Diagrams updated:** none; no container or relationship changed.

## 2026-11-07: A server's name is checked against PVE's rule

**What changed**

- Create, rename and image-from-server check the name with `pve.ValidVMName`, PVE's `dns-name` format plus a DNS name's
  length limits, and answer 400 with what to fix. The create and rename forms apply the same rule as you type.

**Why**

- PVE refuses anything else with a field error, which the API now hides on a 502, so a name with a space read "compute
  backend is unavailable". Only the API and the client can say what is wrong with it.

**Diagrams updated:** none.
## 2026-11-07: Installer re-runs keep operator settings, split mode passes preflight

**What changed**

- Stage 60 on an existing Proxmox provider keeps its storage profiles, default profile and confirmed classes, and only
  adds what was newly detected. A value in the site's group_vars still wins.
- Stage 60 registers the chart versions of the checkout instead of pins in `versions.yml`.
- Preflight accepts `mode: split` and checks its settings. `object_store.mode` must be `vm` or `truenas-app` there.
- Stage 10 requires the CAPI and CSI token secrets in split mode, and takes them from the vault when the tokens exist.

**Why**

- `make upgrade` runs stage 60, and each run replaced those fields with fresh detection. A class confirmed in the admin
  console was lost, and Ceph added to a Standard cluster moved the default profile, so new VMs changed storage with no
  one asking.
- Stage 50 pushes the charts of the checkout. The pin had fallen behind, so a clean split install would register a
  chart version that is not in its registry.
- Preflight still refused split mode with a message from before it existed, so the documented command could not start.

**Diagrams updated:** none; no container or relationship changed.
## 2026-11-07: Protected servers on shared storage

**What changed**

- `ProtectableDisks` takes disks that are all on shared storage as well as all on ZFS. Such a server gets the HA resource alone:
  no replication job, no node-affinity rule, `mode: shared` on its state, and no `server_protection` bill line. A disk that
  would not follow it (`refuseUnreplicable`) is refused either way.

**Why**

- On a Full-tier site every server sits on Ceph, so Protected could not be sold there at all, and HA needs nothing from the
  replication machinery when every node already reaches the disks. There is no replica to bill. Whether to charge for HA
  on shared storage is an operator decision still to be made.

**Diagrams updated:** `c3-components.md` (Replication and HA row).

## 2026-11-07: A node that is down loses no rows

**What changed**

- `Client.ClusterResources` marks VM rows of a node that is not online. `providers.UnreadKeeper` is a new optional provider
  capability: Reconcile keeps a cached row whose object is listed but cannot be read. The server, volume and image sync
  providers implement it. A server on a down node reads `UNKNOWN`, with all else unchanged.
- Power actions on a down node's VM and `pve.IsNodeUnreachable` errors answer 409. `VnetInUse` and `SecurityGroupInUse` refuse
  while a VM of the project cannot be read. A template copy on a down node is not offered.

**Why**

- A node loss drill on Ceph: PVE drops the name of a dead node's VMs after about five minutes, and the sync took a VM with no
  name for one that is not there yet. Servers, volumes and images were archived as deleted while the data sat safely on RBD,
  then came back as new resources when the node returned. Until then the portal said `ACTIVE` and billed a dead VM, a full
  sync wiped its addresses, one unreadable VM stopped the volume sync of its whole project, and a stop answered 200.
- The node is the only thing that knows, so one check where the list is read covers every caller.

**Diagrams updated:** `c3-components.md` (Reconcile row).
## 2026-11-07: Provider actions are audited; a 502 hides its cause from tenants

**What changed**

- Share, managed Kubernetes, managed database and bucket settings actions write an audit event on success, in one place in
  `cloudAction` through `cloudResourceAudit`. Plain reads do not. Taking a kubeconfig or a database's connection info does.
- About 30 sites in `internal/platform/project` stopped returning a raw upstream error on a 502. What the tenant can act on
  stays: a `ConflictError` is a 409 with its text, a full storage is a 409, and PVE's 400 on a firewall rule names the field.

**Why**

- Who changed a bucket policy or took a kubeconfig could not be traced, unlike a DNS record. A wrapper at the dispatch covers
  every action, including those added later.
- Raw errors carried the API's address, and on a proxied call a node's. Some user-facing refusals in `internal/cloud/pve`
  were plain errors relying on that passthrough, so they became `ConflictError`. A test scans the package for new raw sites.

**Diagrams updated:** none.
## 2026-11-07: The console proves a ceph-s3 row before storing it

**What changed**

- `POST /admin/service` and the field-set PUTs run `CheckCephS3` on a `PUBLIC` `ceph-s3` row, for an update only when its
  status, endpoints, region or keys changed. A failed check is a 400 and nothing is stored.
- The Add dialog has a **Test connection** button for `ceph-s3`, calling `POST /admin/service/object-store/test`.

**Why**

- The installer checked before registering, and the console did not. A form typo stored a `PUBLIC` row, which blocks the
  provisioning of every new project. The check sits in the API so that every console path gets it. A write that leaves the
  connection alone skips it, or an RGW outage would stop an operator from turning the service off.

**Diagrams updated:** none.

## 2026-11-07: A broken NAS no longer blocks an address no export lists

**What changed**

- The address release guard takes only what `shareNetworks` can put on an export: public addresses and cluster addresses
  on a shared worker pool. Both now read the same `onExport` rule.

**Why**

- Every cluster address went through the guard, including one on a project network, which the subnet covers. A broken or
  disabled TrueNAS on the project then blocked the release of a cluster with no dataset, against what `docs/managed-k8s.md`
  promises. Addresses an export can list still fail closed, because releasing one would let another tenant inherit access.

**Diagrams updated:** none.
## 2026-11-07: Holders survive a lost answer from PVE

**What changed**

- `KeepServerVolumes` keeps the holder lock until it expires when a move may still be running, the same doubt the carrier
  path already acted on. `taskRefused` is the one rule for telling a refusal from a lost answer.
- `newHolder` on a create POST with no answer removes the VM only if it is in the project's pool and as blank as the create
  leaves it.
- A VOLUME pass looks for a holder on the other nodes before calling it gone. A node that cannot answer fails the pass.

**Why**

- The lock was released with the move possibly running, so a later `dropEmptyHolder` could see the holder empty and destroy
  it as the disk landed. A disk lost that way is customer data, and blocking volume changes in one project for 10 minutes is
  a small price. Only a lost answer gets there.
- The cleanup defer sat after the POST's error check, so a lost answer left an untagged VM in the project pool, billed as a
  server. NextID is racy, so the vmid alone does not make the VM ours: a name, a tag or a disk means it is someone else's.
- `/cluster/resources` names the old node for a moment during a holder's migrate. Skipping that holder archived its volume
  rows until the next sync, up to 15 minutes hidden and unbilled. Keeping rows of every skipped holder instead would have
  kept the phantom carrier rows that the same reconcile exists to remove, so the pass tells moved from gone.

**Diagrams updated:** none (no new component or flow).
## 2026-11-07: Admin delete-now goes through the deletion job

**What changed**

- `DELETE /admin/project/{id}/now` sets the same flag as the client's delete-now, `DELETE_IN_PROGRESS` with a past schedule,
  and returns. The goroutine in `cmd/api` is gone; the deletion job is the one thing that runs `TeardownProject`. The job's
  lock now holds for 30 minutes instead of 1.
- Both cancels refuse a `DELETED` project.

**Why**

- The sweep selects on `scheduledForDeletionAt`, and the admin path set none. A pod restart, an early return from the
  teardown or a failed final save left the project `DELETE_IN_PROGRESS` forever. Stamping the schedule alone would have left
  the goroutine and the sweep tearing down the same project at once, so there is now one runner, and a lock shorter than a
  teardown no longer starts a second one. A project already stuck that way is freed by calling delete-now again.
- A cancel of a `DELETED` project flipped it to `ENABLED` with its cloud resources gone.

**Diagrams updated:** `c4-code.md` (teardown sequence).

## 2026-11-07: Customer image import (P5-11)

**What changed**

- `POST /project/{id}/cloud` `IMAGE {url, ...}`: the portal probes the URL (`pkg/safeurl`, the guard that was private to
  SSO), a node downloads it to `importStorage`, `import-from` builds a template in the project pool. Import dialog on the
  Images page. `ProxCloudPlatform` adds `Sys.AccessNetwork`; the token gets `PVEDatastoreAdmin` on the import store alone.

**Why**

- Tenants could only make images from their own servers. A node fetches the URL from the management network, so the URL
  is the attack surface: the probe refuses non-public addresses on the whole redirect chain and the portal hands the node
  the final URL. A node resolves the name again, so raw images are not accepted and the download goes through PVE's
  untrusted-image check. `Datastore.Allocate` to delete the download is confined to one store, so the token cannot delete
  other volumes.

**Diagrams updated:** `c3-components.md` (new row).
## 2026-11-07: DNS zones cannot overlap across owners; a failed teardown retries

**What changed**

- `powerdns.Service.Create` refuses a name equal to, above or below anything the project does not own, the name server
  hosts included, and a public suffix. It looks again after the create and removes its zone on an overlap.
- `TeardownProject` keeps a project `DELETE_IN_PROGRESS` while a leg asks for a re-run (DNS zones, shares, Proxmox, ceph-s3
  credentials, managed-service remnants), so the deletion job retries. It was marked `DELETED` regardless, and nothing
  looks at that status again.
- Cloud creates and actions refuse a project on its way out, for every provider.
- Tenant errors from a failed DNS or file server no longer carry the raw client error. Record writes are audited with the
  record's name, type and content.

**Why**

- PowerDNS answers from the longest matching zone with no delegation step, so a child zone took over names of a zone it
  did not own, including an operator's. Verified against the lab server through two projects.
- A zone left on the server after a failed teardown kept answering for the former tenant's domain and held its name.
- The raw error named the DNS server's internal address, which tenants must not learn.

**Diagrams updated:** none; the containers and relations are the same.
## 2026-11-07: Holder use is serialised, a carrier is left alone

**What changed**

- A cross-node attach carries the disk on a holder tagged `pc-carrier`, which the regular holder lookup skips. Its
  cleanup runs only once the move is known to have stopped or PVE refused it. Otherwise the empty carrier stays until
  project teardown.
- Creating a detached volume, keeping a server's volumes, attaching and deleting a volume take a per-project lock
  (`ShedLock.Wait`, PostgreSQL). `holderMu` is gone, and `lockShareAccess` uses the same `Wait`.
- A node where the volume's storage is not active is no attach target, and an attach to one is 409.
- `VolumeSyncProvider.ListReadable` skips a VM it cannot read, for callers that only add rows. `List` skips only a holder
  that is gone. A move to another server now ends with a reconcile.
- `newHolder` removes its VM if it fails before the tags are on.

**Why**

- The destroy behind `dropEmptyHolder` takes every disk on the VM when it runs, and the source no longer names a disk
  that has moved. A cleanup racing a running move, or another operation adding a disk, could lose a customer's volume
  for good. A vmid is a cheap price against that.
- A process mutex cannot serialise replicas, and it covered only the lookup.
- An untagged VM in a pool counts as a server, so a failed create left a billed row.
- Retrying a target that could never work moved the disk off its server and answered 502.
- A deleted VM still listed, or a node that is down, failed the one read after an attach and left the volume without a
  row until the next good sync. A sync during the carry could leave a row naming the carrier's disk.

**Diagrams updated:** `c2-containers.md` (attach rule), `c3-components.md` (holder row).
## 2026-11-07: Share exports can keep a site's NAT addresses

**What changed**

- TrueNAS providers take `config.snatSources`, single host addresses, off by default. `truenas.Service` adds them to
  every export it writes or compares, so tenant shares and Kubernetes RWX datasets both keep them through create, sync
  and address release. An empty list takes them off at the next sync. The operator console edits the setting with the
  isolation cost in its help text, and the customer console tells a share's owner when it applies.
- Share rows now carry `updatedAt`, and a stored row without one takes sync updates instead of being refused.

**Why**

- An export lists a project's own addresses. Behind a NAT the NAS sees the NAT's address instead, so the mount failed, and an
  address added by hand was removed by sync, which left mounted pods with `Stale file handle`. A site can now say so
  explicitly. The address is shared by every tenant behind the NAT, so the setting gives up isolation between them on that NAS.
  That is why it is off by default, takes hosts only, and is stated plainly wherever it is set.
- Share rows were written without `updatedAt`, and the sync's optimistic guard refused every update to them. Their
  addresses and `usedBytes` stayed as created, so a share priced on `size_gb` billed its usage at creation.

**Diagrams updated:** `c3-components.md` (TrueNAS client row).

## 2026-11-07: Server backups (P5-3)

**What changed**

- `pve/backup.go` + `project/backup_pve.go`: LIST/CREATE/RESTORE/DELETE_BACKUP on a server, on vzdump to the provider's
  `backupStorage` (NFS, CephFS or PBS). Backups tab in the customer console. `ProxCloudPlatform` adds `VM.Backup`.

**Why**

- Snapshots do not survive a disk or node failure. The ownership proof is a project stamp in the archive's notes, read
  back from PVE on every action, because the store is shared and tenants never reach PVE, so the portal needs no table
  of its own that could drift from the store. Backups go with their server because the token can see and delete an
  archive only through `VM.Backup` on its VM, and an ACL on `/vms` would break the rule that the token only sees pool VMs.

**Diagrams updated:** `c3-components.md` (PVE service).
## 2026-11-07: Protection billed from its first sync

**What changed**

- The first read that sees a first sync done stamps a `pc-p-<unix>` tag, before HA is added. The row carries it as
  `protection.since`, and each protection is its own `server_protection` bill line starting there. A row with no stamp
  has no line. The next read stamps a server protected before this change.

**Why**

- A bill item's first watermark is the resource's creation, so protecting an old server charged the add-on back to the
  server's creation or the start of the cycle, and a disable then enable charged the gap between them. Stamping at the
  enable would still charge the first sync, 73 hours after a three-day one, while the VM has nothing to fail over to. PVE's
  `last_sync` is no use: it is when the sync began (seen on the lab).

**Diagrams updated:** none, no container or component changed.

## 2026-11-07: Grant-blocked IaaS flows run on a real cluster

**What changed**

- A create password always goes through a seed, and a login user (`adminUser`) is rendered by the API from the bcrypt
  hash. The wizard no longer sends the password as plain text user-data, and a typed user-data is no longer dropped.
- A removed seed CD is overwritten with an empty one when the token may not delete it.
- A replication job PVE is still removing no longer counts as protection. Deleting a protected server, or the project
  holding one, waits for pvesr to remove the replica before destroying the VM. A seed CD on node-local storage refuses
  protection. A resize or rebuild of an HA-managed server waits for it to be down first. Test connection names
  `Sys.Console` and `VM.Replicate`.

**Why**

- With the privileges in place, each flow ran for the first time through a portal. Remove protection put HA straight
  back on the VM. PVE wants `Datastore.Allocate` to delete an ISO and the error was ignored, so every seed CD stayed
  forever with its user-data. A password-only server refused that password over ssh. The wizard's login user put the
  password in clear on a CD that another tenant's VM could read from a NAS export covering a subnet. Deleting a
  protected server left its disks on the replica node for good: PVE drops the job with the VM, so nothing removed them.
  A resize left a protected server off: HA took the start request before it had finished the stop.

**Diagrams updated:** none, no container or component changed.

## 2026-11-07: Server snapshots billed (MET-G9)

**What changed**

- The full sync and portal actions stamp `server.snapshots {items, diskGb}` on the SERVER row; the minute sampler
  carries it. `ServerProvider` turns each snapshot into a `server_snapshot` billing resource with the snapshot's own
  start time, priced by a new seed rule. `stampResourceValues` keeps a start time a provider set.

**Why**

- Snapshots hold space on the operator's storage and were free. Proxmox reports no per-snapshot usage on any backend
  (a volume's `used` excludes its snapshots), so the metered size is the provisioned disks: an upper-bound proxy with a
  placeholder rate, until a node-side read justifies real usage. One line per snapshot because a bill item's first
  watermark is the resource's start: a single line per server would back-charge a new snapshot to the server's creation.

**Diagrams updated:** none (the metering flow is unchanged, one more billing resource on the SERVER row).
## 2026-11-07: Split mode keeps the portal's SDN rights

**What changed**

- `mode: split` also grants `ProxCloudPlatform` on `/sdn/zones` to the portal token. Test connection reads `/sdn/zones`
  as well as `/sdn`.

**Why**

- An ACL on a path replaces what its parent passes down. The mint grant alone took `SDN.*` away from the portal, so on
  a split site no tenant network could be created. The lab showed it as soon as the grants were applied. No diagram
  shows ACLs.

**Diagrams updated:** none.

## 2026-11-07: Object store connection checked at registration, public buckets always closable

**What changed**

- New `POST /admin/service/object-store/test`: the API checks a ceph-s3 endpoint and admin key without storing them.
  Installer preflight requires the `ceph-rgw` values, stage 60 calls the test before it writes, updates the row's
  endpoint and key on a re-run, and registers `object_store.region` as the provider's region.
- A bucket's website state now also reads its public-read statement. Disable website removes that statement before it
  touches the website config, and making a bucket private removes it too.

**Why**

- A wrong endpoint or key stored as a PUBLIC provider made every new project fail its bootstrap, after a green install,
  and a re-run skipped the row. Checking from the API tests the network path the running platform will use. The region
  shown to tenants was the Proxmox region name, which RGW rejects as a location constraint.
- The statement publishes objects on the S3 endpoint whether or not the gateway has its website API. A gateway answering
  405, or a provider with no website endpoint, left tenants with a public bucket and no control that closed it.

**Diagrams updated:** none; no container or relationship changed.
## 2026-11-07: Money paths: bill writers under the credit lock, unknown outcomes stay PENDING, unpaid upfront contracts held

**What changed**

- A recorded manual payment and a card charge settling on a bill now hold the profile's credit lock, like paying with
  credits and issuing a bill. A charge the bill already holds changes nothing, and one succeeding on a bill paid some
  other way becomes credit.
- The transaction scanner closes a transaction only when the gateway answers it has no such intent. Any other error
  leaves it `PENDING` with the reason, re-checked until it leaves the 24 hour window.
- An upfront savings contract with no deposit and no operator on record never discounts. Such contracts that are `ACTIVE`
  move to `PENDING_PAYMENT` at API start, and a new operator action records a payment taken outside the portal.

**Why**

- Each bill writer replaces the whole bill it read, so concurrent payments overwrote each other: a race test of 16
  payments of 5 kept 50 of the 80 on the bill. A scanner marking a deposit `FAILED` on a network error needed a manual
  repair if the money had moved, since `FAILED` is terminal, and it could also overwrite a settle that won meanwhile.
  Upfront contracts bought before payments were enforced discounted every bill for money nobody paid.

**Diagrams updated:** none, no container or component changed.
## 2026-11-07: A cluster's addresses and dataset follow the share access rule

**What changed**

- Share exports list a cluster's addresses only on a shared worker pool. A cluster on a project network is covered by
  the subnet. Releasing a cluster's addresses goes through the export guard like a public address.
- Network delete refuses while a live cluster holds addresses on the network, and drops what a deleted cluster left.
- The marker secret is written before a create takes anything, and a failed create releases everything at once. If that
  fails, the sweep retries.
- The release reads the dataset's NAS from the share row instead of scanning every provider, and a failed provider read
  is an error, not "provider gone".
- Chart 0.1.4: `shared-file` PVC directories are named after the PV and are mode `0777`.

**Why**

- Lab drill: deleting a network put the cluster's /32s on the export, where they stayed after the CIDR was free for
  another tenant to take, and the NAS, which refuses a /32 inside a listed subnet, failed every share create and
  refresh on such a project before that. Tokens and datasets of a failed create had no record that could release them.
  A PVC directory named after the PVC handed an old PVC's data, and its pending delete, to a new PVC of the same name.

**Diagrams updated**: none: no container or component changes.

## 2026-11-07: Admin hmac key secrets encrypted at rest, old NFS exports squash root

**What changed**

- `hmac_keys.secretKey` is stored encrypted with the data-at-rest key. Every writer seals it, the SigV4 and MCP lookup
  opens it, and rows in plain text are converted at API start. Creating a key still returns its secret once.
- The share reconcile treats an NFS export with `maproot` as drifted: it opens the top directory and clears `maproot`.

**Why**

- A database read handed over every Admin API credential in the clear. SigV4 has to get the secret back, so a hash like
  the one client tokens use cannot work. A key changed without re-sealing fails closed: the stored text does not start
  working as the secret.
- Root squash only covered new shares, so the oldest ones, with the most data, kept giving a client's root full access
  on the NAS. The top directory opens because otherwise a squashed client could not write to a root-owned dataset at all.
  It is not recursive, so files root wrote before stay owned by root, which `docs/cloud-integration.md` tells operators.

**Diagrams updated:** none, no container or component changed.

## 2026-11-07: Shared block on a TrueNAS LUN checked (P8-4, part)

**What changed**

- Shared LVM on a TrueNAS iSCSI LUN ran as a storage profile: documented as a site setup, marked Implemented. A failed
  volume action now refreshes the project's volume rows.

**Why**

- It needed no code, only proof: detection, profile checks, cross-node clone, live migration and cross-node attach all
  held. A failed attach had left a stale row naming a disk that had moved to a holder, so the retry failed until the
  next sync. The storage plugin, multipath, LINSTOR and democratic-csi remain Planned.

**Diagrams updated:** `c2-containers.md` (storage table).

## 2026-11-07: Tenant DNS zones on PowerDNS (P8-3)

**What changed**

- New provider kind `powerdns` and package `internal/cloud/powerdns`: tenants create zones and edit records in the
  customer console (`DNS_ZONE` rows, `LIST_RECORDS`, `SET_RECORD`, `DELETE_RECORD`). The zone's `account` field names
  the owning project. The sync reconciles rows, and project teardown deletes zones.

**Why**

- DNS was a gap against the earlier cloud backend. A separate provider keeps the DNS server optional and out of the PVE
  path, the same shape as TrueNAS shares. The account field is the one ownership proof, since zone names are global on
  the server.
- The PVE SDN DNS plugin stays Planned: wiring it means changing the shared EVPN zone, and its value depends on IPAM
  records this platform does not create today.

**Diagrams updated:** `c1-context.md` (new external system).

## 2026-11-07: Seeds stay with their project, a retype keeps its row

**What changed**

- A seed is looked up by service, project and vmid. The sync drops seeds whose VM is gone or whose vmid now belongs to
  another project.
- A volume retype repoints the row at the new volid (`cloud.Repo.ChangeExternalID`) instead of archiving it.
- `POST /admin-api/v1/users` without a `sub` makes the Keycloak login, or answers 400 with no service account.

**Why**

- PVE reuses vmids, and a seed holds user-data, so a stale seed could reach another tenant's VM on its next reseed.
- A bill is keyed on the row id, and the same disk showed as deleted next to a new one after a change of type.
- A generated `sub` matched no login, so the created user could never sign in.
- Keycloak calls tenant-chosen SSO URLs out of the API's reach, and DNS can change after the check at save time, so
  the egress policy is now a documented install requirement rather than something the code claims to cover.

**Diagrams updated**: none: no container or component changes.

## 2026-11-07: Money paths: upfront contracts are paid for, credit has one writer per profile

**What changed**

- Upfront savings contracts start `PENDING_PAYMENT` with a server-computed `upfrontAmount`. Only a deposit bound to the
  contract at `AddFunds` activates it, and only `ACTIVE` contracts discount a bill.
- `Repo.WithProfileLock`: pay-from-credit, bill issue and refund read and write credit in one transaction under a
  per-profile advisory lock.
- A deposit's credit has the transaction's id, and the scanner re-drives a deposit stuck `PROCESSING`.
- Bank transfers leave `PENDING` once, through a conditional update.
- A refund refuses a credit partly spent or a contract still running.

**Why**

- A contract was `ACTIVE` at create with no payment, and upfront means usage up to the commitment is free, so a tenant
  got that usage for nothing. Any deposit naming any contract id activated it, whatever the amount or owner.
- Credit was read, checked and replaced with no lock, so two concurrent payers both spent the same balance.
- An advisory lock rather than a row lock: the balance is many rows, and a lock on one profile row would tie the
  money paths to that row existing.
- The settle released its claim on any error after minting, so a retry minted again. A primary key is the one guard that
  also covers a crash between the two writes.

**Diagrams updated**: none: no container or component changes.

## 2026-11-07: Kubernetes RWX on the Standard tier (P7-9)

**What changed**

- With `config.cluster.rwxGiB` set, a new tenant cluster gets one NAS dataset with a `refquota`, the `shared-file`
  StorageClass and csi-driver-nfs, installed by CAAPH. The dataset is an ordinary share of the project named
  `k8s-<clusterId>`, and goes with the cluster. Chart 0.1.3.
- A cluster gets a marker secret, and the orphan sweep keys on it instead of the cloud secret.

**Why**

- proxmox-csi and zfspool are RWO only, and the storage plan puts RWX on the NAS for Standard sites. Using the share
  product means the dataset bills, counts against the share quota and follows the project's addresses with no second
  code path. Full-tier CephFS remains to be built.
- CAPMOX takes ownership of the cloud secret, so it is garbage-collected with the `ProxmoxCluster`. The sweep never saw
  a finished cascade: addresses and minted tokens of a deleted cluster were never released. Found deleting a real one.

**Diagrams updated:** `c2-containers.md` (storage table).

## 2026-11-07: Full tier run on a real Ceph cluster

**What changed**

- A detached volume on shared storage attaches to a server on any node (`carryToNode`): a new holder VM takes the disk,
  migrates offline, and the disk is reassigned on the target's node. `LIST_ATTACH_TARGETS` lists every server of the
  project for such a volume. Local disks keep the same-node rule.
- The stale cloud-init guard on migrate skips shared storage.
- RGW static websites are offered only when `s3WebsiteEndpoint` is set. A gateway without `rgw_enable_static_website`
  (`405`) reports "does not offer static website hosting" instead of an internal error.

**Why**

- The Full tier had only been unit-tested. The plan sells Ceph RBD as attachable on any node, and PVE's reassign refuses
  two nodes even on shared storage, so the code refused it. The refusal was right for local disks only.
- The guard read a shared volume, which every node lists, as a leftover from a failover: no VM with a cloud-init disk
  could live-migrate on RBD or NFS.
- The website tab appeared for every RGW and failed with a bare 500 on a default gateway.

**Diagrams updated:** `c2-containers.md` (storage text).

## 2026-11-07: File share exports follow the project's addresses

**What changed**

- Every release of a public address now passes through one guard (`PublicIPRepo.release`), and network and subnet delete
  through the same one: the address leaves the project's exports first, under a per-project lock, and a NAS that refuses
  fails the release.
- The sync reconciles exports with the project's addresses. Project teardown deletes shares before the addresses go back.
- NFS exports squash root; a dataset is `777`. SMB account names are a hash, stamped on the dataset.

**Why**

- Exports were written only at create and on refresh, so a released address or CIDR reallocated to another tenant
  reached the first tenant's dataset as root. An address is only safe to reallocate once nothing lists it, and the row
  in `pvePublicIp` (or the SDN subnet) is what holds it, so that row goes last.
- The lock exists because a refresh during a release would write the address straight back. The sync reconcile covers
  exports that were already stale when this shipped, and anything edited on the NAS by hand.

**Diagrams updated**: `c3-components.md`.

## 2026-11-07: Protected servers and several API endpoints

**What changed**

- New `internal/cloud/pve/protect.go`: replication jobs, HA resources and node-affinity rules, gating, RPO state on the
  server row, `server_protection` billing, memory reservation in the scheduler, an operator migrate route.
- `config.apiUrls` and `config.caCert`: the PVE client fails over between endpoints and pins the cluster CA.

**Why**

- Local ZFS is pinned to one node; a site that needs a server to survive a node failure had no answer without Ceph.
- HA is added only after the first sync, since a failover before that has no replica to start from.
- Node-affinity rules with equal priority, not `/cluster/ha/groups` (gone in PVE 9): a recovered node must not pull the
  VM back on its own, a second interruption nobody asked for.
- A single `apiUrl` made the node hosting it a point of failure for the whole portal, exactly when HA is needed. Each
  node has its own certificate, so several endpoints need the cluster CA, not a fingerprint.

**Diagrams updated**: `c2-containers.md`, `c3-components.md`.

## 2026-11-07: P5 IaaS parity: server seeds, rebuild, snapshots, subnets, spread groups

**What changed**

- Server seeds (C3b): Planned -> Implemented. A server with user-data, vendor-data or instance metadata gets a
  platform NoCloud seed: a per-VM token URL on the API (`GET /api/v1/nocloud/{token}/{file}`) on a vnet with DHCP,
  a `cidata` CD written by the platform (`pve/seediso.go`) otherwise, or with `metadata.forceIso`. Any other
  server stays on PVE's own drive.
- New actions on a server: `SET_PASSWORD` (guest agent), `REBUILD` (boot-disk swap), `LIST_/CREATE_/ROLLBACK_/DELETE_SNAPSHOT`.
  New on a network: `ADD_SUBNET`, `REMOVE_SUBNET`, `SET_SNAT`. New on create: `spreadGroup`, `spreadStrict`, `metadata`,
  `networkInterfaces[0].subnet`.
- The client gets a public-IP card, a Snapshots tab, a subnets tab and the wizard inputs. The tab that created
  template images is now called Images, for what it does. All behind `/init` feature flags.
- `ProxCloudPlatform` adds `VM.GuestAgent.Unrestricted` and `Datastore.AllocateTemplate`.

**Why**

- A URL seed alone cannot work on EVPN or VLAN: cloud-init fetches it in the network stage, the local stage gives
  up on any CD once it sees a seed URL (lab-verified on cloud-init 25.1), and with no DHCP the guest then has no
  network to fetch with. A CD alone would put user-data on shared storage everywhere and need the upload privilege
  on sites that have DHCP. So the vnet decides.
- The ISO writer is ~150 lines of Go: `user-data` is not a legal ISO 9660 name, so the image needs Joliet, and the
  pure-Go options are whole disk-image libraries.
- Tenant data rides `vendor-data` and `meta`: on one key user-data wins, and a tenant metadata key cannot reach
  `dsmode` or `seedfrom`.
- Rebuild clones first and parks the old disk: a failed clone or move leaves the server as it was.
- A spread group is a tag and a scheduler preference because PVE 9's affinity rules are HA-only and strict.
- There is no router resource: the zone's VRF already routes all of a project's vnets and the isolation rules keep
  other projects out, so a router object would have nothing to decide.

**Diagrams updated**: `c2-containers.md` (Tenant VMs), `c3-components.md` (C3b).
## 2026-11-07: CI opens the built SPAs

Open item of the lab drill (DRILL-1). The same PR fixes console and quota bugs with no architecture
effect.

**What changed**

- New `spa-smoke` job in `test.yml`: builds each SPA, serves the production bundle and opens it in
  Chromium. A load error or a missing login page fails it.

- `ManagementCIDRs` leaves out a node that cannot answer and says so. `ReconcileIsolation` then keeps
  the DROPs in place and still sets the project's ACCEPTs.

**Why**

- tsc and vite build stay green on a bundle that renders a white page, which is how a react and
  react-dom split once reached a running console.
- One node down made every isolation reconcile fail with `595 No route to host`, seen live during a
  node outage. Network create and delete go through the same call, so they failed for every tenant.
  Dropping the failed node's networks instead would have opened a hole if only it knew one.

**Diagrams updated**: `c4-code.md` (C4.1 invariants).

## 2026-11-07: operator Create user makes a Keycloak login

Open item of the lab drill (DRILL-2).

**What changed**

- `POST /admin/user` creates the Keycloak account through `sso.KCAdmin`, keys the row by its id and
  returns a temporary password once. `DELETE /admin/user/{id}` removes the account too.
- Without the service account Create user answers 501 instead of writing a row.

**Why**

- The row had a random `sub` that no login could ever match, so the user could not log in and a later
  self-registration made a second row. Reusing the SSO service account adds no client and no secret:
  it already holds `manage-users` in the customer realm, on compose, the chart and the installer.

**Diagrams updated**: `c3-components.md` (note under C3a).
## 2026-11-07: API tokens get a scope and an expiry

**What changed**

- A personal access token has a scope, `full` or `read`, and an optional expiry. `pkg/auth.ClientKeyLookup`
  returns a `ClientKey`; a read token passes safe methods and the POST list route only.

**Why**

- P5-12 asked for platform API tokens; the tokens already existed. What was missing is limiting one: a token
  in a dashboard or a CI job should not be able to delete servers, or live forever. `/mcp` is refused for a read
  token because its tools can write.

**Diagrams updated**: none, `docs/auth.md` has the rules.

## 2026-11-07: volumes outlive their server (holder VM)

**What changed**

- New `internal/cloud/pve/holder.go`. A volume without a server sits on a per (project, node) holder VM; deleting a
  server moves its data volumes there by default, `?deleteVolumes=true` destroys them. Standalone create and delete.
- Holders are skipped by the SERVER sync and metering (no row, no bill, no quota) and by the scheduler; their disks
  stay VOLUME rows.

**Why**

- A Proxmox disk must belong to a VM, so "delete the server" used to delete data the tenant had detached on purpose.
  The UI warning that stood in for this is gone.
- Keeping volumes by default matches the safe reading of "delete a server": data loss needs an explicit tick.
- A holder per node because PVE reassigns a disk only within a node.

**Diagrams updated**: `c3-components.md` (PVE service).

## 2026-11-07: isolation rules get an owner, cover gateways, and reconcile in the sync job

Open items of the lab drill (DRILL-5, DRILL-6) and the coexistence note in the lab log.

**What changed**

- The sync job reconciles each project's isolation rules every cycle under the `sdn-apply` lock.
- A portal only rewrites DROPs with its own comment: bare `proxcloud:isolation` for the default
  pool prefix, `proxcloud:isolation/<poolPrefix>` otherwise. It only moves ACCEPTs inside its pool.
- `IN DROP` inside the pool, plus an `IN ACCEPT` per project next to its `FORWARD ACCEPT`.
- Rules are deleted by identity with the list's digest instead of by a remembered position.

**Why**

- Rules only reached a project at its next network change, so projects made before a rule change
  kept the old set indefinitely.
- A cluster normally has one portal, and that portal has to keep owning its rules outright so a
  changed pool or blocked list leaves nothing stale. The old code took that to mean every rule with
  the marker, so a second portal with another pool deleted the first one's DROPs at its next network
  change and left those tenants unisolated with no error. Scoping by owner keeps the one-portal
  behaviour and makes the second portal harmless. The default prefix keeps the old comment so no
  running cluster needs a migration.
- A gateway address is local to the node, so traffic to another tenant's gateway is input and no
  `FORWARD` rule sees it.
- PVE addresses rules by position, and a reconcile now runs without anyone asking for it, next to
  operators and possibly another portal editing the same list. A stale position would delete the
  wrong rule, possibly someone's DROP.

**Diagrams updated**: `c4-code.md` (C4.1), `c2-containers.md` (tables).
## 2026-11-07: TrueNAS client and file share product

**What changed**

- New `internal/cloud/truenas`: JSON-RPC 2.0 client over WebSocket and the share service. New provider kind `truenas`
  serving `SHARE` resources, with sync, teardown, a `share` billing resource and a "File shares" page.
- New dependency `github.com/coder/websocket` (ISC) for the client side of the socket.

**Why**

- TrueNAS 26 removes the REST API and revokes a key sent over HTTP, so a REST port would have had no future.
- Tenants on the Standard tier had no shared file product; the NAS is already there as the capacity tier.
- A share is one dataset per project, not the 1/N split across projects of the older design: per-project billing and
  quota come out exact, and no tenant ever shares a filesystem with another.
- Exports list tenant addresses because the NAS only knows a client by its source address. A private subnet behind
  SNAT arrives as a node address, which the NAS cannot tell apart per tenant, so it is left out of the promise.

**Diagrams updated**: `c1-context.md`, `c2-containers.md`, `c3-components.md`.

## 2026-11-07: RustFS tenant buckets behind objectBackend

**What changed**

- `internal/cloud/client`: `Client` now wraps an `objectBackend` with `Caps()`. `cephBackend` keeps RGW;
  the new `rustfsBackend` embeds its plain-S3 data plane. New provider kind `rustfs`
  (`ExternalService.ObjectClient` picks the backend); methods renamed to neutral names.
- RustFS ownership lives in the project's canned policy, the project quota in the API (sum of bucket quotas),
  and scanner usage keeps the last value when the scanner cannot answer.
- Extra S3 keys on RustFS are IAM users granted through bucket policies with a bare-name principal.
- Bucket billing: new attribute `quota_gb` next to `size_gb`; the priced attribute is the billing basis.
- The installer registers the RustFS it deployed as a `rustfs` provider at stage 60.

**Why**

- Standard-tier sites had a deployed RustFS but no tenant bucket product.
- RustFS has no bucket owner and no user quota, so isolation and quota have to live on our side.
- Lab check RUSTFS-POLICY-1 on 1.0.4 showed user principals do work in bucket policies, but only by bare
  name, so grants could keep RGW's model instead of moving to identity policies. The ARN form saves and
  silently matches nothing, which is why the principal form is per backend.
- A tenant able to delete a bucket directly would leave its ARN behind and reach the next owner of the
  name, hence the Deny on the project key and the clean-up of other policies at create.
- Scanner usage is sometimes unavailable; writing 0 would bill nothing for that interval.

**Diagrams updated**: `c1-context.md`, `c2-containers.md`, `c3-components.md` (C3 managed services).
## 2026-11-07: per-organization SSO (PR #45)

**What changed**

- New package `internal/platform/sso`: per-organization identity providers (OIDC and SAML), one
  Keycloak broker per org in the customer realm, created through the Admin API by the service
  account `proxcloud-idp-admin`. Token issuer, SPA and MCP stay as they were.
- Domains verified through DNS TXT, email-first discovery in the client console, JIT membership
  with group to role mapping, enforce-SSO with up to two break-glass owners, an operator list and
  emergency stop, org and operator console pages.
- `pkg/auth.Enforce` gets a guard hook. The SSO guard runs on every request and answers 403
  `sso_*`; on public paths a refused identity continues anonymous.
- `User.ssoAlias` binds a person to the IdP that provisioned them.
- Keycloak 26.8.2 in compose. `deploy/charts/proxcloud/files/keycloak-sso.json` is the realm
  fragment both compose and the chart apply with keycloak-config-cli. The installer turns SSO on
  and generates the service account secret.

**Why**

- Until now SSO meant one broker for every customer of the platform. One realm with a broker per
  org (plan option B) keeps a single issuer, so ADR 0003 holds and SAML comes for free.
- A tenant controls everything its IdP asserts, including the email claim on every login. That is
  why the rules sit in a per-request guard and not only in the JIT step: `users.Require` creates
  users on any route, and a rewritten claim must fail after the first login too.

**Deviations from the plan, found while attacking it**

- Domains: the unique index is on `verifiedDomain`, not `domain`. With a unique domain, anyone
  could squat a name they do not own by claiming it first.
- Account binding is new. Keycloak can end up with a foreign IdP linked to a victim's user (the
  victim follows an email link); the IdP then rewrites the email into its own domain and passes the
  domain gate on the victim's `sub`. The guard refuses an IdP other than the bound one, and for an
  unbound account checks the address ProxCloud already had. Binding state is read fresh: an early
  cached version let a brand-new account through.
- The guard has no exemptions for `/user` and `/account`. One rule is simpler, and the console shows
  the refusal from the 403.
- Enforcement also covers members of the org, not only addresses in its domains. A member could
  otherwise change their email to leave the rule. Enforcing needs a break-glass owner.
- `proxcloud-sso` is a marker scope: while any org has SSO configured, a token without it is refused.
  Without it a self-registered MCP client could drop the mapper and make a brokered session look
  like a local one, skipping the domain gate.
- `SweepForeignUsers` removes Keycloak accounts created by an IdP asserting a foreign address, which
  otherwise block the real owner at "account already exists". This adds `manage-users` to the
  service account. No `manage-realm`.
- Metadata is fetched by the API behind an SSRF guard and handed to Keycloak as a file, so Keycloak
  never dereferences a tenant URL at configuration time.
- No custom first-broker-login flow: Keycloak's stock flow has no automatic link, and the IdP pins it
  by alias. No `verifyEmail` in the chart realm: an install without SMTP could not register anyone,
  and `email_verified` already fails closed.
- Keycloak 26.8 needs the `kc-admin-api` scope on Admin API tokens. keycloak-config-cli has no 26.8
  image; `6.5.1-26.5.5` was verified against 26.8.2.

**Known ceilings**

- A foreign-domain Keycloak account can exist for up to 5 minutes until the sweep.
- Keycloak calls the token and JWKS URLs at login; they are checked when configured, not on each call.
- A domain stays verified if the TXT record is later removed.
- With operators in the customer realm (compose default), brokered sessions are refused on operator
  clients, but that layout is for development only and the API logs a warning.

**Diagrams updated:** c1-context.md, c2-containers.md, c3-components.md (C3a), c4-code.md (sequence d).

## 2026-11-07: payments port (Airwallex, gateway factory, bank-transfer submit, invoices, record payment)

**What changed**

- `payment` gets an Airwallex gateway next to Stripe, selected per payment integration by a
  gateway factory (`payment.NewGateway`). The `Gateway` interface gains refund polling,
  releasing the 1.00 card-verification hold and a credentials ping (the admin health check).
- Card registration is now a small authorization hold that is released once the card is saved.
  Collect creates and confirms the charge in two steps with an idempotency key.
- A deposit confirm claims the PENDING transaction atomically (PENDING to PROCESSING), so
  repeated or concurrent callbacks and the scanner credit once; a settle error hands the claim back.
- Bank transfer, customer side: submit (transfer date, bank reference, note, proof of at most
  2 MiB), cancel, and proof download; finance is mailed on submit. The admin list never carries
  the proof bytes; the proof streams from its own route.
- `billing` gets invoice numbers (`INV-<year>-<seq>`, stamped once on leaving OPEN), invoice and
  credit-note PDFs with the operator as issuer, billing contacts, and a PREPAID or POSTPAID
  payment mode (POSTPAID charges the saved card when a bill is issued).
- Operators record an offline payment on a SENT bill: `POST /admin/bill/{id}/record-payment`
  and the MCP tool `record_bill_payment`.
- Mail carries attachments (the invoice PDF); the `pgdoc` store gains an atomic counter and
  nested transactions join the outer one; four mail templates are added.
- Admin: Airwallex integration form, bank-transfer proof download, record-payment dialog,
  invoice numbers. Client: Airwallex Elements for deposits and cards, bank-transfer panel,
  payment mode, invoice recipients.

**Why**

- A second card gateway removes the dependency on one provider and covers customers Stripe
  does not serve; the factory keeps the services unaware of which provider an integration is.
- Callbacks are unauthenticated redirects. Reading the outcome from the gateway and claiming the
  transaction before crediting is what stops a replayed or raced callback from minting a second
  credit. There is no webhook endpoint, so there is no signature to verify; the gateway is the
  only source of truth.
- Wire transfers are common for business customers, and the operator needs the customer's
  reference and proof to reconcile one; recording a payment closes the loop for payments that
  never touch a gateway.
- Tax invoices need a gapless number and an issuer block; credit notes and recipients are
  required to refund and to reach accounts payable.

**Diagrams updated**: C1 (Airwallex now implemented), C3c (payment, gateway factory, billing,
invoice and credit note rows and nodes).

## 2026-11-07: P7 managed Kubernetes, DBaaS and split mode (branch feat/p7-k8s)

**What changed**

- Managed K8s create on Proxmox (C3e): Planned -> Implemented. New chart `proxmox-kamaji-cluster`; the create path
  resolves flavor, network, addresses and credentials, with one MachineDeployment per PVE node.
- DBaaS create on Proxmox (C3e): Planned -> Implemented. The ten missing actions and create-with-restore are ported;
  endpoints come from a MetalLB pool, replicas spread hard across zones, backups go to any S3 endpoint.
- Services cluster, Cluster API + CAPMOX, MetalLB (C2): Planned -> Implemented. Installer stage 50, `mode: split`.
- K8s node template pipeline (C2): Planned -> Implemented, with image-builder.
- proxmox-csi disks (`-pvc-` volumes) no longer sync as the project's volumes.
- Applications are listed per provider label, since one cluster can host both a kamaji and a dbaas provider.
- Databases moved to `pc-db-<projectId>` namespaces: their default-deny NetworkPolicy cut tenant control planes in
  `pc-<projectId>` off from their workers when both shared a cluster.
- Removed from the add-on menu until they are wired: node problem detector, monitoring, NVIDIA GPU operator, reloader.
- Node group autoscaling is refused, and GPU flavors for node groups.

**Why**

- Pinned pools rather than a cloud controller manager: PVE clones to local storage only on the template's node, so
  workers are pinned to a node anyway, and the pin gives the zone label without another token in the tenant cluster.
- The worker's route to the PVE API goes through a Service on the services subnet, since tenant vnets are blocked from
  the management network, and its CSI token is scoped per cluster because a tenant can read it.
- The manager-level CAPMOX credential stays empty because it silently overrides `credentialsRef`: that is what
  upstream issue #513's error turned out to be here. The cloud-init check is off because on PVE 9 it needs
  `VM.GuestAgent.Unrestricted`, which is root in every guest.

**Diagrams:** C2 (containers, deployment), C3e.

## 2026-11-07: P4 metering foundations (branch feat/p4-metering)

**What changed**

- Proxmox state sampler (C3c): Planned -> Implemented. `pveStateSync`, every minute: one
  `/cluster/resources` page per provider reconciles SERVER, IMAGE and FLOATING_IP rows of every
  project, carrying agent IPs and the GPU count from the cached row.
- Proxmox task poller (C3c): Planned -> Implemented. `pveTaskPoll`, every 20 seconds, per-node
  cursor, UPID dedupe; re-syncs the project, audits other users' tasks, and backs `LIST_EVENTS`
  (was an empty stub).
- VM identity: portal-created VMs carry a `pc-c-<unix>` tag. A row whose identity changed is
  archived and a new one inserted in the same pass, disks included, so a reused vmid gets a new
  row and bill item. The portal also archives a row older than its own create.
- Traffic (C3c): Planned -> Implemented. `metrics.PVEFetcher` reads public-NIC counters from
  `status/current` every minute into `nicCounter` with reset-aware deltas. `pve` is the new default
  source of a Proxmox provider.
- Public IP and image billing: Planned -> Implemented. FLOATING_IP rows derive from the allocations
  (one per allocation, deletable, which detaches the NIC); new `image` resource type on `size_gb`.
- GPU count comes from `hostpciN mapping=` on the full sync, tags being only the fallback.
- Rating engine: a started unit is a billed unit, `getTimeUnitsDiff` floors at 0, rows archived this
  cycle are fed back with `DeletedAt`, `OVERWRITE_TOTAL` is stored bare on the first charge too.
- Staleness policy (C3c): new. Last-known state for 30 minutes, then the provider's resources stop
  billing, the gap is written off (`SkipInterval`, new on the `billingapi` wire contract), and an
  `ERROR` log line and a `METERING_STALE` audit event fire once.
- Admin API refuses a rule pricing a usage attribute unless it is `OVERWRITE_TOTAL` on an hour or
  minute cadence (an unnamed apply method defaults to overwrite).
- Removed: the inert `load_balancer` price type (provider, catalog entry, seed rule).

**Why**

The cache was refreshed every 15 minutes and keyed on the vmid alone, so state changes outside
the portal lagged charges, and PVE's reuse of the lowest free vmid let a new VM inherit the old VM's
row and bill item. Hour rules lost the first partial hour and the last interval of every
resource. One page per provider instead of 3N calls makes a 60-second cadence affordable, and
the 2-minute grace exists because pmxcfs lags: without it a just-created VM would be archived and a
just-deleted one resurrected. The stamp replaced `meta: ctime` as the identity because a clone keeps
its template's ctime (verified on PVE 9.2), so ctime only separates VMs created from scratch. A
started hour bills in full so that a resource's bill is a simple count of clock hours; truncating
would make any VM younger than an hour free. The write-off exists because `NotEligibleForBilling`
alone only delays a charge: the watermark would stay and recovery would back-bill the whole outage.
Status/current was picked over a metric-server push because it needs nothing installed on the
nodes; its ceiling is the traffic between the last sample and a counter reset.

An external billing service must mirror the rating changes (`getTimeUnitsDiff`, the first
`OVERWRITE_TOTAL` charge, `SkipInterval`).

**Diagrams updated:** C3c, C3b table.

## 2026-11-07: tenant isolation moves to cluster firewall rules, EVPN gets platform IPAM

Found by the end-to-end lab drill on PVE 9.2.

**What changed**

- Tenant isolation is now a managed rule set in the cluster firewall (`proxcloud:isolation`
  rules, one ACCEPT per project) instead of per-vnet firewall rules. It also drops traffic
  from the pool to the nodes' management networks and a new `network.blockedCidrs`.
- On a zone without DHCP (EVPN, VLAN) the platform allocates each server's `net0` address,
  sets it through cloud-init and pins `ipfilter-net0` to the `/32`. The allocation rows live
  in `pvePublicIp` on NIC `net0`; only `net1` rows count against the `floatingIps` quota.
- A public NIC takes the default route; attach and detach answer `rebootRequired`.
- `ResolveVM` reads power state and config from the node, not from `cluster/resources`.
- A failed project deletion stays `DELETE_IN_PROGRESS` and is retried, and `DELETE
  /project/{id}/now` now actually deletes.

**Why**

- Vnet `FORWARD` rules only see frames switched inside one vnet bridge. Traffic between
  subnets is routed by the node, so with the old rules one tenant reached another, and
  through an exit node's leaked VRF route also the node management network and storage.
- PVE refuses `dhcp` on an EVPN zone: network create failed outright, and DHCP is simple-zone
  only. A static address also lets ipfilter pin the exact address, which closes the
  spoof-a-sibling ceiling.
- With a default route on both NICs, replies from a public address left through the overlay.
- `cluster/resources` lags a finished task by seconds, so a resize right after a stop
  restarted the VM.
- A rollback to `ENABLED` after a half-done teardown showed a broken project as healthy and
  was never retried.

**Diagrams updated**: `c4-code.md` (C4.1, C4.2, sequences).

## 2026-11-07: P6 installer and image pipelines

**What changed**

- Installer (C2c): Planned -> Implemented. `installer/` is one Ansible entrypoint,
  `make install INVENTORY=...`, in `allinone` mode: preflight, Proxmox identity, NAS stores,
  OS templates, k3s (`mgmt.placement: ssh | pve-vm`, always `--cluster-init`), the platform
  through ArgoCD Applications with inline values, the object store, and registration through
  the admin API. Day-2: `upgrade`, `templates`, `storage-rebench`, `add-node`, `remove-node`,
  `mgmt-scale`, `rustfs-upgrade`, `rustfs-rollback`. `split` mode stays Planned with P7.
- VM template pipeline (C2c): Planned -> Implemented. `images/build.py` (vendor image, Packer,
  smoke test, publish by tag, prune) runs from the installer and from
  `.github/workflows/vm-templates.yml`, which has no PR or fork trigger.
- Template discovery (C3b): new. The catalog comes from cluster tags (`pc-public`,
  `pc-img-*`, `pc-v-*`) in pool `pc-templates`; `config.templates` is now an optional pin or
  overlay. Server create clones the target node's own copy, so a local-disk site is no longer
  pinned to one node.
- `proxcloud-api admin-key create`: new. A non-interactive Admin-API key, minted through the
  database; the one writer path (`admin.NewAdminAPIKey`) is shared with the endpoint and the
  debug trigger.
- Chart: CloudNativePG and a new `RabbitmqCluster` are the defaults, the bitnami subcharts
  are opt-in (`values-dev.yaml`). `cnpg.keycloak` and `auth.*.discoveryUri` added.
- RustFS (C2): Planned -> Implemented as the internal object store; tenant buckets stay
  Planned. Platform-owned VMs (C2b): Planned -> Implemented, except the services cluster.
- New `ProxCloudImages` role and `images@pve!packer` token.

**Why**

- A first install took hand-run `pveum`, hand-made templates and psql seeds. The plan's
  v3.1 decision is one command with the easiest default for every choice, each overridable.
- Tags, not config, because a weekly rebuild changes template ids: a catalog in config goes
  stale on every run. The pool is the ownership proof, a tag alone is not.
- Per-node copies, because PVE clones across nodes only from shared to shared storage (the
  P3 lesson): with local VM disks, one template meant every VM on one node.
- `ProxCloudImages` uses `Sys.AccessNetwork` instead of `Sys.Modify` on `/`, and has no
  ACL on `/vms`, so the pipeline token cannot touch a tenant VM.
- k3s's bundled Traefik and ServiceLB replace MetalLB and nginx-gateway-fabric in `allinone`:
  one host needs no VIP pool, and every extra component is one more thing to pin and upgrade.
  They return with the services cluster.
- cert-manager is always deployed, not only for ACME, because the RabbitMQ operator's webhook
  needs it (operator v2.23).
- The operator's own Secret carries the RabbitMQ credentials, so there is one less secret
  to generate and rotate. The API reads it directly.
- The admin realm is separate from the customer realm: any login in the admin realm is an
  administrator, and the customer realm allows self-registration.

**Diagrams updated:** c1-context.md, c2-containers.md (C2b, C2c, tables), c3-components.md (C3b)

## 2026-11-07: console and server detail match what Proxmox serves

No component changed. The wizard honors the boot volume type, offers the project's own
images and sends one network; the serial console speaks the `termproxy` framing; PVE write
calls run on a context detached from the client connection, because a browser going away
mid-clone left a half-built VM. Fields the backend ignored were removed rather than kept
(fixed IP, DNS and gateway on networks, volume name, security group description, ether type).

## 2026-10-28: P3 storage foundation and honest UI (branch feat/p3-storage, PR TBD)

Summary of the P3 slices below (P3-1 to P3-10); the per-slice entries carry the detail.

**What changed**

- Capability detection (C3b): Planned -> Implemented. `pve/capability.go` derives each
  storage's type, sharing, content, snapshot/thin/live-migrate/replication support, tier,
  default class and per-node free space from the cluster; it runs every sync cycle, on admin
  Re-detect and on every profile save. Admin class override per storage; the installer's
  fio result is a suggestion only.
- Storage profiles are validated against detection (flags may only downgrade; parsed flags
  are also clamped to the detected capabilities on every read, so a profile written on
  provider create cannot claim more), gain a
  derived tier and class, a `label`, and `bootDisk` / `dbaas` caps. A capacity-class
  profile never takes boot disks, databases or snapshots.
- Server create: placement restricted to nodes where the profile's storage is active and
  keeps `storageHeadroomPct` (default 10 %) free; a template on shared storage clones to a
  picked node whose profile storage is shared too (PVE clones cross-node only shared to
  shared), a local template or a local profile pins to the template's node; refusals are
  409 with the reason.
- Volumes: add-volume free-space check, same-node attach gating (PVE's disk reassign needs
  both VMs on one node), `LIST_ATTACH_TARGETS`,
  `RETYPE` via `move_disk`, delete of detached disks; per-disk `profile` and
  `storage_class` reach billing.
- Client: `/init` `features` per provider and `platformConfiguration.hiddenServices`; the
  client hides actions the backend would reject and warns on capacity types and on server
  delete.
- Docs: `cloud-integration.md` (new *Storage capabilities and placement* section, provider
  document, accessors, sync job, create sequence), `configuration.md` (provider storage
  keys), `data-model.md`, `glossary.md`, `architecture.md`, the admin guide
  *Connecting a Proxmox VE Cluster* (new *Storage* section) and the client guides
  *Block Volumes* and *Working with Servers*.

**Why**

- Cross-node clones from a template on local storage, and cross-node attaches of local
  disks, failed inside Proxmox with raw 502s after the platform had already committed to a
  node.
- The scheduler ignored free space, so a nearly full pool was picked and the clone failed
  late (or filled the pool).
- The client offered actions every Proxmox request rejected or silently ignored (rebuild,
  rescue, set password, user-data, boot-time data volumes, ports, subnets, volume
  snapshots, attach to any server).
- Nothing stopped tenants from putting boot disks, databases or snapshots on the HDD NAS,
  whose RAIDZ HDD plus sync NFS gives single-disk IOPS, and operators could not price it
  apart.

**Diagrams updated:** c2-containers.md (storage tiers table and text), c3-components.md
(C3b capability detection, Scheduler, PVE service, Sync job, Provider config, Project
cloud handlers, Platform config, Billing resources, DBaaS rows), c4-code.md (sequence a:
create server)

## 2026-10-28: Capacity HDD (NFS) profile and capability gating (P3-9, S18)

**What changed**

- Storage profiles gain a derived `tier` (full only when every storage is Ceph) and
  `class` (the lowest effective class of their storages), an optional `label`, and the
  `bootDisk` / `dbaas` caps (`false` downgrades). A capacity-class profile never takes
  boot disks, databases or snapshots; a profile save claiming them is refused (400).
- API gates: server create on a profile without boot disks is 409; `RETYPE` of a boot
  disk onto one is 409; an image (server snapshot) of a server with an attached disk on
  capacity storage is 409; a database create whose storage class is named after a
  profile closed to databases is 409.
- The volume sync stamps each disk's own profile and storage class (`volume_type` now
  follows the disk's profile, not the VM's). The volume billing resource exposes
  `profile` and `storage_class`, so price rules can price "capacity" separately.
- `LIST_VOLUME_TYPES` returns `displayName`, `tier`, `class`, `bootDisk` and `dbaas`.
  Client volume create and retype label capacity types "HDD, not for databases" and
  warn; the admin profile editor flags capacity profiles.

**Why**

- The HDD NAS is the Standard tier's capacity store, never the hot VM-disk tier: RAIDZ
  HDD with sync NFS gives single-disk IOPS. Tenants must not land boot disks, databases
  or snapshots there, and operators need to price it apart.

**Diagrams updated:** c2-containers.md (storage tiers table and text), c3-components.md
(Provider config, Billing resources and DBaaS rows)

## 2026-10-28: Honest client UI and managed-service switch (P3-6, P3-7, P3-8)

**What changed**

- `GET /init/{projectId}` gains `features`: per attached provider id, the optional actions
  its backend serves (`cloudFeatures`). Proxmox serves `serverSnapshots` (image from a
  server) and `volumeRetype` (only with two or more storage profiles). No backend serves
  rebuild, rescue, set password, user-data, boot-time data volumes, ports, subnets or
  volume snapshots yet, so the client hides them.
- `platformConfiguration.hiddenServices` (service slugs) is dropped from the `/init` menu,
  so the client hides Kubernetes, Databases or Object storage entirely. The admin
  System > Platform page has the toggles; providers keep syncing.
- Client: server delete dialogs warn that every volume of the server is deleted, detached
  ones included; Create volume picks the server; Attach lists `LIST_ATTACH_TARGETS`;
  Delete shows only for detached volumes; the server Network tab shows the guest-reported
  addresses when the provider has no ports.

**Why**

- The client offered actions every Proxmox request rejected (or silently ignored, such as
  user-data and boot-time data volumes), and operators had no way to hide a managed service
  that is not ready to sell.

**Diagrams updated:** c3-components.md (Platform config and Project cloud handlers rows)

## 2026-10-28: Volume attach gating, retype and delete (P3-4, P3-5)

**What changed**

- Attach to a server on another node is refused with 409: qemu-server's disk reassign
  (`move_disk target-vmid=`) dies with "Both VMs need to be on the same node" even on shared
  storage. A target with no free disk slot is 409 too. New volume action
  `LIST_ATTACH_TARGETS` returns the servers on the volume's node (none while it is
  attached).
- `RETYPE` (`newType` = profile slug) moves the disk with `move_disk storage=<profile
  storage on the VM's node> delete=1`. The target must hold VM disks, be active on that
  node and keep the headroom free; otherwise 409. The old cache row is archived, the sync
  adds the new volid.
- `DELETE` of a volume destroys a detached disk (drops its `unusedN` entry); an attached
  one answers 409 "Detach the volume before deleting it". Volume create still needs
  `serverId` (400).
- New `pve.ConflictError`, `Service.AttachTargets`, `Service.RetypeVolume`,
  `Service.DeleteVolume`; handlers map `ConflictError` to 409 (`pveVolumeErr`).

**Why**

- Cross-node attach of a local disk failed inside PVE with a raw 502, there was no way to
  learn which servers were eligible, and detached disks could neither be deleted nor moved
  to another volume type.

**Diagrams updated:** c2-containers.md (storage tiers text), c3-components.md (PVE service
row)

## 2026-10-28: Template placement and capacity-aware scheduling (P3-1, P3-2)

**What changed**

- Server create reads live storage capabilities and the template's config. A template whose
  volumes (disks, EFI/TPM state, cloud-init and ISO drives) all sit on shared storage is
  full-cloned with `target=<picked node>` and `storage=<profile storage on that node>`
  only when that profile storage is shared as well: qemu-server's clone dies with "can't
  clone to non-shared storage" for a cross-node clone onto local storage. A template with
  any volume on local storage, or a local profile, is pinned to the template's node; when
  the storage profile is not active there the create answers 409 with the reason instead
  of a 502 at clone time.
- Scheduler (`PickNode`) skips a node whose profile storage is inactive or would keep less
  than `config.storageHeadroomPct` (default 10 % of its capacity) free after the root disk.
  Data-volume create (`AddVolume`) runs the same check on the server's node and answers 409.
- `PVEStorageProfile.Nodes` + `pve.ActiveStorageNodes` replaced by
  `PVEStorageProfile.Placement(caps)` (node -> storage state); new `pve.StorageHasRoom`,
  `pve.TemplateShared`, `pve.ErrStorageFull`; `AddVolume` takes the headroom.

**Why**

- Cross-node creates from a local template failed inside PVE with a raw 502, and the
  scheduler ignored free space, so a full pool was picked and the clone failed late.

**Diagrams updated:** c2-containers.md (storage tiers text), c3-components.md (Scheduler
row), c4-code.md (server create sequence)

## 2026-10-28: Storage capability detection (P3-3, P3-10 API part)

**What changed**

- Capability detection (C3b): Planned -> Implemented. New `internal/cloud/pve/capability.go`
  derives per storage, from `GET /storage` and `GET /cluster/resources?type=storage`: type,
  shared, content, snapshots, thin, live migrate, replication (`zfspool` only), tier
  (`full` for rbd/cephfs, else `standard`), a default class from a static type table, and
  per-node active/total/avail.
- Provider config gains `config.capabilities` (written by detection: every services-sync
  cycle, the new admin `POST /admin/service/{id}/storage/detect`, and every storage-profile
  save) and `config.storageClasses.<id>` (`class` = admin override, set through the new
  `PUT /admin/service/{id}/storage-classes`; `suggestedClass` = installer fio result, shown
  only).
- Behaviour change: a `storageProfiles` save on a Proxmox provider is validated against a
  live detection (storage exists, active on its node(s), has `images` content, flags only
  downgraded). A cluster that cannot be reached rejects the save.
- Behaviour change: `PVEStorageProfile.Nodes` takes the active storage map; `*` now means
  "nodes where the storage is active" instead of "all nodes", and explicit node entries are
  dropped when their storage is inactive there. A server create on a profile with no active
  storage answers 409 instead of trying a node that cannot hold the disk.
- Admin UI: provider Storage tab (capabilities, active nodes, detected/suggested class,
  override select, Re-detect button).

**Why**

- Profile flags were admin-declared and unchecked, so a profile could claim shared or
  replication on local storage, or map a storage that is disabled on a node, and placement
  failed late at clone time. Detection gives the honest baseline the P3-4 attach gating and
  P3-9 caps enforcement build on.

**Diagrams updated:** c2-containers.md (storage tiers), c3-components.md (C3b diagram and
table), c4-code.md (server create sequence)

## 2026-10-28: Remove OpenStack remnants

**What changed**

- Dead OpenStack code paths are deleted: the gophercloud dependency, the Swift half
  of the object-store client, the `OS_*` / `openstack.*` config keys (the default
  region is now `PROXCLOUD_DEFAULT_REGION` / `proxcloud.default-region`, default
  `default`), the stale `/api/v1/notifications/` public-path entry, and the
  OpenStack-only client, admin and mgmt routes (`/debug/cloud`, metadata, service
  auth and details, discovery, project imports, unassociated projects, placement,
  reseller, availability zones, share protocols, VHI-ostor, Gnocchi granularity,
  volume-type writes) with their MCP tools and admin UI tabs.
- Persisted names now match the backend: table `gnocchiMetrics` is `usageMetrics`,
  job `gnocchiMetricsFetch` is `usageMetricsFetch`, download types
  `OPENSTACK_SWIFT_BUCKET` / `OPENSTACK_CONSOLE` are `OBJECT` / `CONSOLE`
  (`OPENSTACK_S3_OBJECT` is gone), the Gnocchi metrics source and the two Ceilometer
  Prometheus schemas are removed (only `libvirt-exporter` remains, pending a Proxmox
  schema).
- Public Admin API: `configuration.cloud.provider` reports the real provider kind
  (`PROXMOX`, `CEPH_S3`, `KAMAJI`, `DBAAS`) with a `proxmox.pool_prefix` block;
  `provisioned_services[]` carries `provider`, `region` and `pool` instead of the
  `openstack` block, and the `openstack_project_id` filter is gone.
- Managed K8s and DBaaS (C3e): the create-path stubs are renamed
  `cloud_kamaji_create.go` / `cloud_dbaas_create.go`; Cinder CSI, Octavia
  annotations, keystone application credentials, Glance image ids and neutron RBAC
  plumbing are removed, with one `TODO(P7)` at each seam naming the Proxmox piece
  (CAPMOX, proxmox-csi, MetalLB). Read, sync and delete are unchanged.
- CI: the rebrand gate also fails on OpenStack-era terms (this file and
  `countries.json` excluded).

**Why**

- **Misleading operators and API clients.** Dead paths and OpenStack wire fields
  (a fixed `OPENSTACK` provider value, empty `openstack` blocks, admin tabs and MCP
  tools that changed nothing on Proxmox) suggested features that do not exist and
  hid the real provider, pool and region. Names now match the Proxmox backend.
- **No installed base.** There are no deployed installs, so tables, jobs and enum
  values are renamed outright, with no compatibility shims.

**Diagrams updated:** c3-components.md (C3e stub file names). No diagram shape
changed.

## 2026-10-28: PR #16 tenant isolation, VM firewall, quota hardening

**What changed**

- Tenant networks (C2b, C3b, C4.1): every vnet now carries managed forward rules
  that accept the project's own subnets and drop anything else headed into the
  provider's `cidrPool`. New isolation mode `evpn-per-project` gives each project
  its own EVPN zone (id `ZoneIDFor(projectId)`); `shared` stays the default.
- Network create (C4 sequence b): the CIDR is picked under the platform `sdn-apply`
  lock, must sit inside `cidrPool` and must not overlap any subnet on the cluster.
  Isolation rules are written before the SDN apply. `cidrPool` is now required on
  Proxmox providers (breaking).
- VM firewall (C4.2, sequence a): `firewall=1` on every NIC, ipfilter and macfilter
  anti-spoof, inbound default DROP, a per-project `default` security group created at
  bootstrap. Cloned NICs beyond `net0` are removed. The firewall is set before the
  first boot; a failure destroys the VM. Server create now requires one of the
  project's networks (breaking).
- Public IPs (C2b, C3b, C4.2): new public IP pool per public network (`pvePublicIp`
  table) with a `floatingIps` quota; the address is pinned by ipfilter; the
  management bridge is refused.
- Test connection (C2 trust boundaries): refuses a cluster with the datacenter
  firewall off or a node on the legacy iptables firewall; warns on missing token
  privileges and on `root@pam` tokens. Least-privilege roles documented in
  `docs/proxmox-permissions.md`.
- Quota (C3b, C4.3): the provider default quota is seeded onto new projects; the
  gate fails closed (503) when usage cannot be read; resize, volume create and
  extend, and the boot disk are counted.
- Teardown (C4 sequence c): project teardown also removes the project's vnets,
  isolation rules, EVPN zone and security groups, and releases its public IPs. The
  scheduled deletion job now runs the same teardown (it used the old cloud write
  path, which never reached Proxmox resources).
- Identity (C3a, C2 trust boundaries): `email_verified` is read from the token
  and mirrored to `emailConfirmedAt`; org add-member and project invites match
  IdP-verified emails only.

**Why**

- **Cross-tenant reachability.** All tenant vnets shared one SDN zone with no rules
  between them, so a tenant VM could reach another tenant's VMs. The zone routes
  between its vnets; only firewall rules (or a separate VRF plus rules) stop it.
- **Spoofing.** Without `firewall=1`, ipfilter and macfilter, a VM could send with
  any source address, including another tenant's or a public address it did not
  hold. A template made from a VM with a public NIC would also have handed that NIC
  and its address filter to every clone.
- **Unbounded and untracked resources.** Public addresses had no pool and no record
  of who held them, and a failed usage read let a create through. Without a default
  quota a new project could consume the whole cluster. Quota now bounds what is not
  yet billed (public IPs, templates) and fails closed.
- **Account takeover through unverified email.** Anyone can register an account with
  an email address they do not own. Matching people by unverified email let such an
  account be added to an organization or claim a project invite meant for the real
  owner.
- **Silent misconfiguration.** With the datacenter firewall off, or a node on the
  iptables firewall, every rule above is silently unenforced; the preflight turns
  that into an error.

**Diagrams updated:** c1-context.md, c2-containers.md (C2b, trust boundaries),
c3-components.md (C3a, C3b), c4-code.md (C4.1 to C4.3, sequences a, b, c).

## 2026-10-28: Initial C4 model (baseline: initial ProxCloud release)

**What changed**

- Baseline of the model as of the initial ProxCloud release: a multi-tenant
  private-cloud portal and billing system for Proxmox VE. One Go API
  (`/api/v1`, SigV4 `/admin-api/v1`, `/mcp`, SSE, management port), customer and
  operator React consoles, PostgreSQL (JSONB document store), RabbitMQ, Keycloak,
  Helm charts and GitHub Actions CI publishing to ghcr.io.
- The IaaS backend is Proxmox VE (`internal/cloud/pve`): pools per project, full
  clones, SDN networks, cluster-firewall security groups, storage profiles, GPU PCI
  mappings. Tenant buckets run on Ceph RGW.
- Code base rebranded to ProxCloud (module, `PROXCLOUD_*` config, image names), with a
  CI gate that rejects old brand strings.
- Planned elements drawn from the roadmap: installer, Proxmox native metering
  samplers, HA and replication, TrueNAS and RustFS for the Standard storage tier,
  Proxmox Backup Server, per-org SSO, managed Kubernetes and DBaaS create on Proxmox,
  VM and node template pipelines, Airwallex.

**Removed (not drawn)**

- The OpenStack compute, network and identity backend; only the Ceph RGW bucket
  client survives in `internal/cloud/client`.
- The OpenStack notification relay (notifier); resource state now comes from the
  15-minute Proxmox sync.
- OpenStack-only managed-service legs: managed Kubernetes and DBaaS create on
  OpenStack are stubs that fail with an explicit error until the Proxmox versions land.
- Gnocchi as a metrics source; traffic metering uses an optional
  Prometheus-compatible source, off by default for Proxmox providers.

**Why**

- ProxCloud targets Proxmox VE private clouds. Keeping the OpenStack paths would
  have meant shipping code with no backend to run against; the bucket surface was
  backend-agnostic and stayed.

**Diagrams updated:** all four files created.
