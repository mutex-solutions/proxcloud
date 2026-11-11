# Runtime CREATE image pins (bounded P2-B1 integration)

## What this adds—and what it does not

Native sandbox CREATE (console/MCP through the native API) and E2B CREATE now use
one shared consumer protocol for an owned managed immutable template image. This
closes the gap between selecting a ready custom image and recording a running
consumer. A provider **builtin pointing into the managed namespace is protected
too**, even when the request has no Custom template.

This is **CREATE-only participation, not complete runtime coverage or GC
permission**. Resume, fork, wake, saved/paused images, snapshot producers,
provider/catalog writers and legacy adoption are separate work. The generic
readiness/resume path is unchanged. There is no public proof/skip/completeness
flag, retry/takeover route, background re-dispatch or clear-hold endpoint.
Both physical registry-delete paths remain unconditional HOLD.

> **Availability warning:** this slice deliberately retains Pending, Bound or
> Unknown pins after physical CR/pod deletion, terminal Pod phase, NotFound,
> timeout and successful DELETE. Repeated creates can therefore accumulate durable
> storage holds and block cooperative pruning indefinitely. Do not clear these by
> age or delete their owner rows. Safe reclamation requires a separately reviewed
> protocol that retires the desired CR generation and excludes old Apply requests
> and controller recreation. B1 does not implement that exclusion and never calls
> `ReleaseRuntimeTerminal`.

## C4 context and containers

- **Tenant/client:** chooses template alias, size, environment and network policy;
  cannot select a sandbox ID, operation UUID, nonce, proof or source classification.
- **Project application:** resolves an early candidate, freezes the effective
  validated request, owns the standalone runtime-start state machine, and invokes
  only the two CREATE-scoped readiness callers.
- **Sandbox service:** verifies and consumes the opaque proof before any Kubernetes
  call, renders the actual CR, then independently observes its owning live Pod.
- **Registry pin engine:** DB-only BeginPins/factory/typed-observation operations on
  a writable PostgreSQL PRIMARY. Its public API is unchanged.
- **PostgreSQL:** persists the standalone `sandbox_runtime_start` owner, pin and
  dispatch marker. The runtime owner survives deletion/archive of billing rows.
- **Kubernetes controller and Pod:** external effects; neither ReadyCandidate nor
  an API response is sufficient runtime binding evidence.

```text
native CREATE ─┐
              ├─ candidate + original requested alias/tag
E2B CREATE ───┘   -> frozen validated/defaulted Spec + reserved server sandbox ID
                  -> standalone PREPARING owner [UUID + nonce + hashes]
                  -> standalone BeginPins [ONE exact immutable image; committed]
                  -> lock host namespace -> current project -> provider/template
                     -> revalidate exact source/settings/revision/spec -> PINNED
                  -> standalone AuthorizeRuntimeDispatch [one opaque proof]
                  -> owner epoch CAS DISPATCHING
                  -> existing withSandboxStart rotation fence
                     -> low-level Consume(full spec), DB-free, once
                     -> namespace/quota/list -> CR + pod-template annotations
                     -> sole Apply
                  -> CREATE-only WaitReady + current end/deadline check
                  -> independently GET current CR + actual owned Pod
                  -> owner first-UID/epoch CAS OBSERVING
                  -> RecordRuntimePod -> Pending-to-Bound observation
                  -> exact owner CAS OBSERVED -> cache -> bounded usable response
```

The existing rotation transaction around the cluster callback is a compatibility
exception, not a new network-in-SQL abstraction. Pin acquisition and proof factory
work finish **before** it. Owned and other-DB inherited transactions are rejected;
no context is stripped to manufacture an independent commit.

## Candidate, source policy and fingerprint version 1

The original native template or E2B `templateID` alias/tag is retained **before**
the existing resolver substitutes the canonical name. The candidate records the
owned template/build IDs, existing generation if present, immutable image, all
inherited settings and a stable candidate hash. Legacy builds without Registry
metadata do not get an invented generation: their build ID/settings/hash remain
part of exact revalidation. Moving an alias, changing inherited settings, deleting
a template, or changing the current project/provider causes zero dispatch; the
request never silently follows a replacement generation.

The only managed source is the literal canonical
`HOST/t/currentProject/20lowerhexTemplate@sha256:64lowerhexDigest`, parsed by the
engine's strict parser. Foreign project paths, internal tags, combined tag/digest,
URLs and malformed/noncanonical internal spellings fail closed. Only trusted
operator-curated library/external template sources skip outside the engine; a
custom template cannot manufacture an external skip.

One pure sandbox helper computes the same fingerprint for the application and
low-level guard. It validates/defaults an immutable deep copy and explicitly
includes fields that Spec JSON omits: server ID, project, Custom template, quota,
source candidate, effective image/start/ready/command/user/workdir/root settings,
GPU/resources, environment and metadata, lifetime/autopause, network/traffic
policy, effective egress, runtime class/envd/startup/limits and provider scope.
The provider connection contributes only an aggregate hash. Environment values
participate in canonical sorted JSON hashing **but no raw environment or request
payload is stored**. Server envd/traffic tokens are minted only after Consume and
are excluded from this hash.

The provider owner hash includes an opaque revision computed from the stored
**encrypted** provider document before decryption. A secret-only change, including
removing a wake token, invalidates the old candidate even if public config is
unchanged. Neither ciphertext nor plaintext credentials are persisted in the
runtime owner; the revision is transient on the provider and excluded from JSON.

## State, uncertainty and binding

PREPARING, PINNED and DISPATCHING transitions use an exact operation/source/spec/
provider/epoch CAS. Pending protects the gap before the later owner/factory commits.
Receipts are stored using explicit nested operation IDs: an embedded Pin/Operation
JSON ID-name collision must not discard either the pin ID or operation UUID after
a database round trip.

Positive refusal before the factory—or positive cancellation of its locally
UNUSED proof—may commit a non-reusable failed tombstone and release its precise
unbound Pending receipt. Rotation refusal before the callback follows that rule.
A used/lost/ambiguous proof cannot be canceled. Apply errors, lost commit responses,
caller disconnects and cache/readiness failures retain Pending/Bound/Unknown, with
fixed operator-hold diagnostics and a server-generated operation ID. There is no
retry under that operation, no remint after restart and no claim of a durable
proof-used bit. Value copies share the proof's same local once bit.

A successful binding requires fresh current CR and Pod identities, ownership
labels, project/service/sandbox scope, CR UID ownerReference with controller=true,
Running phase, literal actual main image on CR and Pod, immutable operation/nonce/
mode/image/spec-hash annotations on CR, pod template and actual Pod, and a matching
runtime imageID digest. Missing imageID is a hold, not invented provenance.
ReadyCandidate and cached payloads cannot substitute for these reads.

The first UID is attached atomically in OBSERVING before privileged Record/Bind.
Stale DISPATCHING errors cannot overwrite that source guard or turn a newer
successful binding Unknown. Concurrent same-envelope observers join only the exact
UID/spec/next-epoch Bound result by read-only lookup; another UID cannot join or
poison it. A crash in OBSERVING retains a nonexpiring conservative hold.
Bound remains held for the whole consumer lifetime **and after physical cleanup**.
The core terminal API has a necessary positive-terminal evidence condition, but
caller-side generation retirement/controller exclusion is separately necessary.

## Local verification and operational boundary

The unit tests cover immutable copies, ignored public JSON fields, source policy,
fingerprint sensitivity and proof-required failures before any Kubernetes access.
Opt-in real PostgreSQL tests use two independent loopback pools and a fake
Kubernetes API, never a lab registry/provider. They cover both public CREATE paths,
committed pins before rotation/callback/first namespace call, source/provider drift,
inherited transactions, unused cancellation versus ambiguous Apply, value-copy
one-send behavior, receipt identity after restart, malicious live evidence,
concurrent observer joins, prune exclusion in both orders and holds after deletion.

Example for an already-owned disposable loopback PostgreSQL:

```sh
RUNTIME_CREATE_TEST_POSTGRES='postgres://u:p@127.0.0.1:PORT/runtime_create?sslmode=disable' \
  go test -race ./internal/platform/project -run '^TestRuntimeCreatePostgres' -count=1
```

Tests do not start or pull a PostgreSQL container themselves. Startup schema Ensure
runs outside every transaction, including helper-first-use compositions. Production
wires the explicit PRIMARY runtime owner store; missing wiring cannot silently
disable managed CREATE protection. Any rollout and controlled acceptance remain
separate approval gates; implementing B1 alone does not enable deletion.
