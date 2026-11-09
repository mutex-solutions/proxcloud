# Sandbox API port: contracts and safety boundaries

## Provenance and scope

The API feature port follows the upstream sandbox API at commit
`2f443b5d4720bc6571f39c1280285909acaf1eb4` (the four-commit feature series ending
there), adapted onto the target's independently reviewed registry BUILD baseline.
The source checkout is read-only. This page describes API behavior, not deployment
approval, complete registry-writer coverage, or permission to run live operations.

## Context and containers

- **SDK / CLI** authenticates `/e2b` calls using a project sandbox API key. Every
  request rechecks the creator's project cloud-resource management permission.
- **Customer console** uses its session principal and the project in the route.
  All template routes, including GETs, intentionally require
  `PROJECT_CLOUD_RESOURCE_MANAGE`; no browser API key or key store is required.
- **Project API** reserves names, worker admission, immutable settings and operation
  nonce on the primary PostgreSQL database. Alias names identify a template;
  version tags identify one of its builds. These are separate namespaces/contracts.
- **Registry pin ledger** commits current-own managed source/destination receipts
  before any worker can use a reference. BUILD and IMPORT have distinct opaque,
  single-use dispatch proofs; their operation UUID cannot authorize both actions.
- **Sandbox service** revalidates trusted provider policy and dispatches a fixed
  import worker or a build worker. The import worker never runs a customer's image
  or Dockerfile. It verifies immutable manifest/config/layers and runtime pull access.
- **Sandbox runtime** reports phased startup. The first committed Ready observation
  fixes the runtime clock; cache coverage is not readiness.

## Immutable import sequence

1. Validate source digest, project identity, runtime settings and the operator's
   exact origin/repository policy. Verify the trusted digest-pinned worker exists.
2. Observe live workers outside SQL, then reserve names and durable admission under
   the project name lock. An identical source/settings key reuses the same operation
   and adds same-project aliases; it never redispatches an uncertain operation.
3. Commit pins outside any inherited SQL transaction. Reattach exact receipts using
   the operation/owner/generation CAS, then commit one-shot import authorization.
4. Consume the opaque proof before external dispatch. The existing provider-wide
   rotation fence remains in place. An unused proof can be invalidated and released;
   a consumed proof plus timeout/apply error becomes an irreversible UNKNOWN hold.
5. Read the actual import pod UID, project/service/import labels and frozen
   source/destination/op/nonce. A positive terminal phase is necessary but does not
   prove an image digest. Validate the trusted result independently.
6. Persist terminal evidence, transfer the destination (or the single source digest
   for verify-in-place) into a durable template binding, then publish usable READY.
   Retry only the idempotent handoff; never retry external dispatch.
7. Delete a confirmed-terminal worker only after the result is durable. Unknown or
   active workers retain their operation and receipts.

Managed verification reads only an immutable current-project
`<configured-host>/t/<project>/<20-lowercase-hex>@sha256:<digest>` reference with
that project's pull identity. Its destination is the same bare repository; no new
image is built or copied. External copy uses an operator-approved exact repository
and caller-owned source credentials, writing a unique current-project destination
build tag before verifying the resulting digest with runtime pull credentials.

## Concurrent mutation and safe representations

New template reservations, imports and alias edits share the project name lock.
Deletion first commits a tombstone under that lock; it performs no registry/pod I/O
inside the name transaction. Nonexpiring retired build identities prevent stale
saves from resurrecting removed generations. Generic saves cannot advance frozen
BUILD or IMPORT operation state. Public status/default/tag selection uses effective
READY, not a raw terminal-observed `ready` field before receipt handoff finishes.

The console projects explicit safe DTOs rather than serializing stored documents.
Upload/fetch tokens, source credentials, image ENV, import provenance keys, pin
receipts and operation nonce/state are excluded. Diagnostics redact known credential
forms; callers must still not print arbitrary secrets in RUN output. Paged log
positions are stable indexes into a bounded immutable prefix, with explicit loss and
truncation markers and a separately bounded terminal tail.

## Startup, cancellation and retention

Create, native create, resume, fork and wake use the same phased startup policy.
The API retains an outer deadline and checks cancellation after readiness, cache
persistence and access retrieval; no late `201` is published. Failed startup cleanup
uses a detached but finite 30-second context. Resume ownership covers snapshot wait
plus the maximum startup budget. An ambiguous resume is not taken over merely
because a lease elapsed, `Alive` succeeded, or a later GET returned not found.

Build cancellation is durable compensation intent, **not** a public cancel endpoint.
The console's Cancel button only closes its recipe dialog. UNKNOWN operations cannot
be cleared by cleanup, a missing pod, a deadline or a new claim. Successful/tagged
artifact history is retained until an explicit reference-aware history deletion
contract exists; only unreferenced cleaned failed history is bounded.

## Deliberate limits and compatibility exceptions

Both physical registry manifest-delete paths remain unconditional **HOLD**. This
port integrates new imports in the current internal GC scope, not every provider,
catalog, legacy image writer or adoption path. Existing retained From/Image checks
remain conservative cleanup holds; they are not claimed as complete provenance.
No prune enabling flag, grace override or provider/catalog coverage claim is added.

New project admission callbacks contain no cluster scans or apply calls. The
pre-existing provider-wide `withSandboxStart` SQL transaction/rotation-lock callback
still spans external create/resume/build/import dispatch as a documented compatibility
exception; removing it requires a separately reviewed durable rotation/start lease.
The new pin acquisition and authorization occur before that callback and reject
inherited transactions rather than stripping transaction context.

Preparation is default-off and reports observed eligible-node N/M image-cache
coverage. Import/warming capabilities are false when the trusted worker, policy,
runtime capability or durable protection is unavailable. No deployment manifests or
operator environment are changed by the API lane.
