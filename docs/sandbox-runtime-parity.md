# Sandbox runtime parity: EE source port, ProxCloud boundaries

This core port follows the read-only source pin `2f443b5d4720bc6571f39c1280285909acaf1eb4`
(`9f33f11` through four source commits), on ProxCloud's approved P2-A main history.
It does not deploy a runtime, publish an image, enable warming/imports, or authorize
physical registry deletion. Both real registry-delete entrypoints remain on HOLD.

## Intentional startup/TTL migration

Startup is **not runtime TTL**. `Config.Startup` defaults to 900 seconds total,
120 scheduling, 720 image-pull and 600 runtime initialization; each is clamped to
1–1800 and no phase exceeds the total. Each Kubernetes read gets the remaining
current-phase deadline, bounded by the caller/total context. Returned results are
checked again after reads: a late, uncooperative reader cannot return usable Ready.

New creates and armed saved resumes/forks have a finite provisional deadline of
startup budget + requested runtime TTL (plus the existing auto-pause shutdown
grace). A pending sandbox does not auto-pause or bill. A private UID/resourceVersion
CAS first persists a trusted Ready candidate. A second CAS atomically commits
`runningSince`, Ready-based runtime end and clears startup-pending. Failed final
patches retry from the same durable candidate, even if the Ready condition flaps.
A replacement UID, expired original provisional lifetime or expired candidate
runtime never extends the lifetime. A Ready condition by itself is not usable and
cannot bill while startup-pending remains set.

“First Ready” means the first **durably recorded trusted Ready observation**.
When every candidate write fails, no system can prove an unrecorded historical
observation after a crash; this implementation returns unavailable rather than
claiming it. Once recorded, the candidate/origin is never refreshed by a restart.
`ready` remains independent from billable `status=RUNNING`: previously running,
currently NotReady sandboxes stay billable but are not handed out as usable.

`runtimeClock=ready` marks these new records so disappearance before readiness
does not manufacture an assumed start. Legacy unmarked records retain their old
assumed-start behavior and the existing minute-aligned billing arithmetic.
Saved startup arming preserves its provisional deadline through resume. Callers
must retain the final caller-context/lifetime checks around access/cache responses.

## Environment and PID1

Caller limits: 256 variables, 32 KiB/value, 128 KiB total including `NAME=value\0`.
`PROXCLOUD_*`, `KUBERNETES_*` and `E2B_SANDBOX_ID` remain reserved. Values/NUL are
checked without including values in errors. The server replaces duplicate, stale
or `valueFrom` sandbox IDs on create/resume/fork. Image ENV cannot override it.
PID1 checks merged image/pod environment against a 256 KiB limit, Linux per-string
and actual stack-dependent exec limits, reserving 32 KiB for argv/runtime defaults.

Explicit `--serve` pod args suppress image CMD selecting `-ready`/`-snapshot`.
Utility modes cannot run as PID1. The existing native PID1 wrapper, read-only
runtime mount, envd token restart readiness probe, UID checks, abuse/exec activity,
routing/egress controls and configured runtime sizing limits remain intact.

## Independent builder resources and diagnostics

`Config.Build` is separate from runtime sizing. Defaults are 2 CPU/4 GiB RAM/30 GiB
disk; safe default maxima are 7 CPU/24 GiB/60 GiB, at most two concurrent workers.
Dedicated pool selectors and larger explicit caps are operator decisions, not an
oversubscription promise. Disk1 is valid: all stage requests stay at or below the
configured disk limit (scheduler request is min(2 GiB, limit)).

Target OCI invariants remain: force OCI output, directory-layout crane push,
credential-free `prepare-output` root helper (drop ALL, RO root, only output mounted),
physical-tree validation, and nonroot push with read-only output. Logs/stages include
`prepare-output`; the EE tar-based handoff is deliberately not copied. Stable bounded
128 KiB stage prefixes, separate bounded terminal tails, truncation/unavailability,
resource/exit/signal/OOM evidence and common-credential redaction support the API's
durable archive. Raw kubelet scheduling messages are not exposed. Listing no longer
deletes finished workers before the API archives diagnostics. Mere timeout,
absence or Unknown is never positive terminal evidence or cancellation permission.
P2-A's opaque single-use dispatch proof/operation/nonce/no-replay guards are preserved.

## Trusted immutable imports

There are no EE registry aliases or shared source robot fallbacks. Operator
`Config.Import.SourceRepositories` maps canonical HTTPS origins (optional port,
no userinfo/path/query/fragment) to **exact repositories**, optionally substituting
the validated current `{project}` ID. Empty policy admits only
`<configured-host>/t/<current-project>/<20hex-template>@sha256:<64hex>`.

Managed verification uses only that project's pull identity, scoped in the worker
to the exact repository. External copy requires an operator-listed exact repository
and an explicit caller auth document; anonymous `{auths:{}}` additionally requires
`AllowAnonymous=true`. Source, push and runtime verification identities are separate.
No ambient credential chain is used. Cross-repository mounts, foreign layers,
redirects, foreign token realms/scopes and nested-repository upload locations are
rejected before credential-bearing transport. Exact registry endpoint grammar is
checked, not just a path prefix. Proxy environment is disabled.

The trusted image must be supplied by the operator-owned
`sandbox-system/sandbox-import-worker` Secret key `image`, pinned to
`<configured-host>/library/proxcloud-sandbox-import@sha256:<64hex>`.
Without that image/policy/project pull identity/permissions the feature is unavailable.
The policy is a fixed bounded JSON worker argument derived from operator config,
not caller-provided allowed URLs. The worker never executes images or unpacks layers;
it verifies linux/amd64 manifest/config/digests/descriptors/compressed-size budget,
then uses the runtime pull identity to verify the destination config and every blob.
External copy pushes the exact authorized tag; result images are repository@digest.

`StartImport` consumes a distinct durable `ImportProof` before Kubernetes writes.
Only independently read UID/operation/nonce/source/destination/positive terminal
phase can settle the operation. Unknown writes are retained, never replayed or
blindly deleted. Imported workers never enter Kaniko compute billing. This does not
claim complete registry-writer participation: trusted snapshot assembly and other
legacy writers remain separate prerequisites for eventual GC authorization.

## Optional warming and capability prerequisites

Warming is off by default. It admits only immutable current-project images, bounds
catalog count, conservatively reserved size/total, TTL, active parallelism and selected
nodes (default16, hard128). Every reconciliation revalidates **current** policy,
limits, image size reservation, node pool, capability and readiness; lowering caps,
changing selectors or disabling retires only owned intents/pull pods. It never
removes runtime images or containerd snapshots. Kubelet cache inventories—not old
successful pods—report observed coverage and trigger bounded retries after eviction.
There is no sub-five-second cold-start/cache guarantee.

Local queued admission is context-aware; a resourceVersion lease fences replicas.
Service-scoped names, project/service labels and UID-precondition deletion prevent
cross-service cleanup. Pull workers run only the read-only trusted static
`sandbox-init -prepull`, under gVisor, RO root/dropALL, no envd/user command/token/CRI
socket. The protected label
`node-restriction.kubernetes.io/proxcloud-prepull-runtime=v1` is granted **only by an
operator after canary-verifying the installed binary**. Node setup does not auto-label.
Remove the label before downgrading runtime. The bundle capability marker is evidence,
not an authorization or proof that an old live node has been upgraded.

Per-project `sb-<project>/sandbox-template-pull` is copied only into trusted worker
namespaces with project/service ownership labels; never revive a shared pull robot.
Optional manifests document needed namespace-scoped RBAC and deny pod-network egress
for pull-only workers. No manifests, node labels, credentials or image publications
were applied as part of this source port.

## Local evidence

Focused Go1.25.14 unit/race suites exercise durable candidate retries/UID conflict,
post-read caller/phase/lifetime failures, no-pending billing, environment boundaries,
OCI output invariants, exact import policies/credential redaction, malicious upload
locations via loopback registry tests, warming limits/node inventory/config changes,
service isolation and cancellation-aware admission. Integrated API/SPA/full-CI and
any controlled runtime canary are independent parent-owned acceptance gates.
