# VM inventory safety and repair

## What changed and why

PVE filters both `/cluster/resources` and pool members by the configured token's
visibility. A nonempty partial page is no safer than `200 []`. Pool.Audit can
survive a VM purge while VM.Audit disappears;403 then means unknown, not absent.
Full and sampled SERVER/VOLUME/IMAGE reconciliation now share a pre-write proof.
Ordinary positively listed members require current configuration, pending state,
SMBIOS UUID, matching pool/node/type and complete usable payloads. An empty type
with no cache is a no-op, not a reason to demand a global reader.

Unknown/partial/malformed/offline/migration/restore/permission results keep exact
cache rows, IDs, timestamps and history. One failure stops that scope before any
cache mutation and suppresses dependent destructive work. Protection projection
is read-only; automatic healing is deferred until project proof succeeds.
CSI-owned volume rows remain gateway-owned; moving/carrier and unresolved durable
holder/seed intent never become deletion authority. Legacy SERVER rows learn UUID
from a full strict mapping before sampled enrichment can be carried forward.

## Proving real retirement

A would-disappear or changed-identity decision additionally requires **the same
configured provider credential**, with already-authorized Sys.Audit on `/access`
(full ACL tree) and `/` (independent cluster membership), propagated VM.Audit at
VM/pool namespace and every ACL shadow, and node audit. Stable bracketed pool,
VM, configuration and permission reads must agree. All cluster members must be
online/quorate for absence. Only canonical exact-node/VM missing-config500 or an
exact target-specific masked400 is absence evidence under that authority; generic
404,403, null and unrelated missing dependencies are not.

No new grants are installed. A root/seed-reader result cannot substitute for the
configured credential. Narrow installations continue normal known refreshes but
retain a truly deleted VM if they cannot prove its absence. Native explicit DELETE
continues its existing positive owned-task retirement path, unchanged.

## Orphan allocation safety and limits

Cleanup runs **after** successful SERVER and IMAGE reconciliation. A fresh complete
VM absence proof also scans every visible global VM/template current+pending
configuration for allocation references. LXC, offline/locked/unreadable config,
unknown durable operations, or **any other guest NIC** veto automatic release:
static in-guest addressing cannot be disproved from an IP configuration page.
UUID reuse, BeforeRow allocations and allocations without an original cached
server snapshot remain held for manual reconciliation.

A short primary PostgreSQL transaction then takes the same allocation lock as
allocation, checks that no new cache generation appeared, checks the exact full-row
archive receipt and row-locks/compares **all** allocation JSON before deleting its
exact ID. No provider I/O runs inside this transaction. Native/legacy histories
without the optional receipt never authorize automatic release. An allocation or
server mutation with an unchanged updatedAt also invalidates old evidence.

This conservative automatic branch converges only when full authority, no other
potential NIC user and exact archive/allocation evidence exist. On a populated
cluster operators should expect retained addresses, not assume global absence of
one VM is IP non-use. Finite read proofs do not fence external PVE administrators;
there is no distributed PVE CAS claim.

## Operator repair

1. Inspect the warning's provider/project/type and preserve its cache/history/IP
   allocation; do not purge rows, backdate grace or clear encrypted user data.
2. Read effective rights **as that exact token**, including user/token intersections
   and more-specific NoAccess ACLs. Pool membership omissions alone are not proof.
3. Restore only the permissions the owner explicitly intended, or keep the narrow
   token and reconcile the exact deleted object/address manually using positive
   owned-task and global current/pending configuration evidence. Do not silently
   assign a global auditor or use a second root token.
4. Resolve durable holder/seed/migration intent before retries. Re-run sync with a
   fresh invocation; failure state is invocation-local. A recovered known refresh
   preserves existing row IDs rather than resurrecting guessed deletions.

## Local evidence

Go1.25 focused tests use a disposable Docker PostgreSQL17 and loopback PVE wire
server enforcing one exact token, GET-only and no `userid`. They cover two unknown
rounds, empty/partial pages,403/masked400/NoAccess/pool schema, offline/migration,
templates/holders/reuse, malformed config/pending/NIC/disk/page fields, complete
canonical absence, ordinary refresh and empty IMAGE without global ACL reads,
whole-row same-updatedAt CAS, CSI/moving/carrier preservation, snapshot/protection/
detached-size failures, pure-list mutation counters, legacy UUID learning,
foreign-NIC IP retention, BeforeRow regression, exact allocation generation and
archive Info/updatedAt receipt races. These are hermetic evidence, not an actual
PVE ACL/node-failure drill. No live ACL, node, network, VM, email or timer changes
are part of this PR.
