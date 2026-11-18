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
holder/seed intent never become deletion authority. A malformed or missing guest-agent
result is unknown, not an empty address set: cached dynamic addresses remain exact,
including legacy rows that have not learned UUID yet. Template flags, when present,
must be numeric0 or1 before any SERVER/IMAGE classification decision. Legacy SERVER rows learn UUID
from a full strict mapping before sampled enrichment can be carried forward.

## Proving real retirement

A would-disappear decision (a cached row whose VM is not in the project pool any
more) requires that **no guest in the whole cluster holds the row's VMID**:
`GET /cluster/nextid?vmid=<id>` with the same configured provider credential.
Only the answer Proxmox gives for a free id counts, a 2xx whose `data` is that
id; null, an empty body or another value keeps the row, like any refusal other
than `VM <id> already exists`.
Proxmox answers it from the cluster's guest list for any authenticated user,
whatever the token may audit. It needs no grant and a filtered page cannot fool
it: a VM that was moved out of the project pool, is hidden by an ACL, or sits in
a pool the token has `NoAccess` on still holds its VMID, and its row stays. One
held VMID also counts as gone: the token reads the VM that holds it, in another
pool, and its SMBIOS UUID is not the row's. Proxmox hands out the lowest free
VMID, so a VM of another project takes the number of a deleted one. A
changed-identity decision (the same VMID in the project pool under another SMBIOS
UUID, a server that became a template, a disk gone from a current config) rests
on the VM the token read itself. Both also need Sys.Audit on `/` (independent
cluster membership), every member online and the cluster quorate, and stable
bracketed pool, VM and configuration reads; the VMID is asked again after them.

Until v0.8.0 both decisions asked for a view of every guest instead: Sys.Audit
on `/access` (the full ACL tree), propagated VM.Audit on `/vms`, `/pool` and
every ACL path below them. The documented grants
([proxmox-permissions.md](proxmox-permissions.md)) have no ACL on `/vms` or
`/access` and `NoAccess` on the fenced pools, so the proof failed at every sync
and no SERVER or VOLUME row was ever retired: a deleted cluster's worker stayed
ACTIVE in the project and counted against its quota (lab finding F60). That
proof is kept for the address release below only, where another guest's NIC
matters, and there it still never passes on the documented grants.

A held VMID keeps the row, and the warning says which:

- `VM exists outside the expected pool`: the token sees the guest in another
  pool, and it has the row's SMBIOS UUID (the VM itself, moved), or the UUID of
  one of the two is unknown (a row from before UUIDs were stored, a container,
  a locked config). Move the VM back, or reconcile its owner by hand.
- `a guest this token cannot see holds the VMID`, with the path: `/pool/<pool>`
  when the token cannot show propagated VM.Audit on the project pool (repair
  that grant), else `/vms/<id>`: the VM was moved out of the token's pools or
  hidden by an ACL, or a guest outside them took the number (on a
  managed-services install, a machine of the platform).

In the last case the token cannot tell its own VM from a foreign one, so the row
stays, and one such row stops that project's SERVER sync and what follows it
(volumes, networks, isolation rules) before anything is changed, at every run,
until the guest that holds the number is gone. The sync's warning for it ends
with the cause above; it no longer says to repair token permissions, because
here there is nothing to repair: do not add a `/vms` ACL and do not remove a
fence row for it.

When the old VM is known to be gone, delete the server through the API
(`DELETE /api/v1/project/{id}/cloud/{resourceId}`). Read from the code, not run
on a lab: every step of that delete (backups, protection, kept volumes, stop,
destroy) looks the VM up by VMID in the project pool among the guests the token
sees, finds none and does nothing in Proxmox; the handler then drops the
server's seed token and CD, gives back the addresses recorded for that server
id, archives the VOLUME rows that name it and archives the row. The same route
serves the row of a managed cluster's worker (the lab's stale row was one): a
worker's row is an ordinary SERVER row of the project and the route has no
check on it. Only a load balancer's VM row is refused (409: delete the load
balancer). If the guest that took the number is one the token sees in another
pool, the delete is refused instead (`vm <id> is not in project pool`), and
that case is the `VM exists outside the expected pool` one above.

No new grants are installed. A root/seed-reader result cannot substitute for the
configured credential. Native explicit DELETE continues its existing positive
owned-task retirement path, unchanged.

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

This conservative automatic branch still asks for the view of every guest
(Sys.Audit on `/access`, propagated VM.Audit on `/vms`, `/pool` and every ACL
path below them), which the documented grants do not have: on them the address
of a retired server is kept until an operator releases it. It converges only when full authority, no other
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
rounds, empty/partial pages,403/masked400/NoAccess/pool schema, the documented
grant set with the pool fence (a free VMID retires, a held one does not), offline/migration,
templates/holders/reuse, malformed config/pending/NIC/disk/page fields, complete
canonical absence, ordinary refresh and empty IMAGE without global ACL reads,
whole-row same-updatedAt CAS, CSI/moving/carrier preservation, snapshot/protection/
detached-size failures, pure-list mutation counters, legacy UUID learning,
foreign-NIC IP retention, BeforeRow regression, exact allocation generation and
archive Info/updatedAt receipt races. These are hermetic evidence, not an actual
PVE ACL/node-failure drill. No live ACL, node, network, VM, email or timer changes
are part of this PR.
