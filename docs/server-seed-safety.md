# Ordinary server seed CD safety (Phase 1)

This is **not** CAPMOX bootstrap ISO handling and does not sanitize live user-data after boot.
A server's CD is still plaintext on its shared storage. Protect that storage from every tenant path,
including public-NIC routing, SNAT and NFS exports. Keep shared storage for migration/HA; changing the
seed to node-local or HTTP on a static network would break existing lifecycle semantics.

## What changed and why

`pveSeedIdentity` holds the installation's random identity. `pveSeedArtifact` commits an exact
`pcseed-v1-<installation-128bits>-<artifact-128bits>.iso` name **before** uploading. DB identity plus
an exact random artifact row is ownership authority, not the prefix or VMID. The journal contains no
payload or credentials. Retain these tables with database backups; do not clone a portal DB into a
second active writer against the same cluster. Missing identity or mismatched rows are never adopted.

The active `pveServerSeed` remains the encrypted rebuild source. Its iid changes only on explicit
rebuild; ordinary NIC reseeding keeps it. Retiring an explicitly deleted server revokes delivery but
preserves ciphertext until cleanup succeeds. Incomplete global visibility or a missing cache row
cannot strip an active source. A stale source read cannot overwrite a newer iid/ISO under the seed lock.

Every delete or empty-CD fallback goes through one gate:

1. Exact canonical owned name + journal row for this installation/provider.
2. No uncertain upload/attach/delete task. Journal `mutationPending=true` is committed before mutation;
   a timeout or process crash remains operator-reconciliation work, not a timer-based delete.
3. Same quorate cluster name/membership/IP identity and storage-definition hash as creation. Missing
   original identity permits normal creation but disables automatic cleanup. Legitimate membership or
   storage edits can conservatively retain old artifacts; never rewrite evidence blindly to force cleanup.
   Cluster name/membership is a drift fingerprint, not a cryptographically unique cluster UUID. Replacing
   a cluster with identical names/IPs/backing under the same trusted endpoint remains coordinated operator
   work; restoring old DB/storage together must not be used to reuse a retired filename.
4. Complete all-VM/all-template current, pending and snapshot config reference scans. Any matching random
   filename on **any** storage vetoes, including a physical NFS/CephFS alias. No DNS resolution or guessing
   storage aliases is needed. LXC/unknown guest types conservatively block this first implementation.
5. Two no-reference observations separated by 24 hours and an immediate fresh recheck. Observed reference,
   ACL change, migration, malformed/null response or failed read resets evidence and prevents erasure.

The PostgreSQL connection-level advisory lock plus non-expiring `pveSeedOperation` claim serialize this
provider's seed writers and cleanup across replicas, with progress committed independently. The durable
claim prevents a second holder entering if the advisory-lock connection dies while PVE work continues.
A crashed callback/failed claim release blocks later operations until operator task reconciliation; no
lease expiry guesses safety. Any unknown seed task also blocks other artifacts/generations of that VMID.
Product backup/restore commits a `pveSeedRecoveryPin` under this lock **before dispatch**. Any such pin
retains **all artifacts of the provider** (another VM's archive can name the CD); it intentionally survives
DELETE_BACKUP and VMID reuse. Operators must prove all archives/tasks gone before clearing it. This
conservative availability/storage tradeoff avoids pretending current configs cover backup references.
The lock does **not** coordinate independent portals or external administrators. PVE DELETE/overwrite has no atomic unreferenced-file CAS. Privileged external
operators must coordinate seed attachments and cluster/storage reconfiguration; this is a trusted-ops
boundary, not an atomic global safety guarantee. Never attach or reuse a retired random artifact name.

## Optional complete inventory reader

Default: no reader configured, therefore no destructive cleanup. The usual portal token intentionally
cannot see system VMs; its successful empty list is not proof. Operator provisioning is explicit;
neither installer nor API silently mints tokens or broadens the portal's grants.

Use a **different** user and privilege-separated API token, kept services-side with these propagated
read grants on both user and token (exact effective privileges must survive all child ACLs):

| Path | Required read privilege |
|---|---|
| `/` | `Sys.Audit` |
| `/access` | `Sys.Audit` (full ACL tree visibility) |
| `/nodes` | `Sys.Audit` |
| `/vms`, `/pool` | `VM.Audit` |
| `/storage` | `Datastore.Audit` |

No `Permissions.Modify`, storage allocation, console, guest-agent, VM config or power privileges are
needed by the reader. This is broad **read** access: VM configs can contain seed URLs/password hashes;
trust it accordingly and never expose it to guests. The client uses only GET with the same provider
endpoint/CA/fingerprint policy, never a new URL or remote-exec helper.

Configure through the existing provider advanced JSON/API read-modify-write surface:
`config.metadata.seedInventoryTokenId` and **encrypted** `secret.seedInventoryTokenSecret`.
Do not put the secret in config, shell arguments, screenshots, logs or documentation. Preserve the rest
of metadata and secret fields. Token rotation does not change artifact ownership.

Completeness proof deliberately does not use the no-path `/access/permissions` map: PVE omits empty
NoAccess shadows there. It verifies exact `/access` Sys.Audit, reads the full `/access/acl`, then checks
exact effective propagated audit rights at **every** VM/pool/storage ACL path, including unrelated
principals' paths and restrictive children; each listed VMID is checked too. Lack of any grant blocks
cleanup. Confirm coverage against an independent root read-only manifest, including a VM outside all
portal project pools, before considering this operationally enabled. No new grant is applied here.

## Operator status and recovery

`GET /api/v1/admin/service/{id}/seed-cleanup` requires `admin:service:read`. It returns reader-configured,
attached/pending/emptied/deleted counts, pending artifact identities, provider recovery pins, operation
start/state and fixed/sanitized reasons—no seed contents, full VM config, token or credential. It is a read-only status, not an erase/adopt endpoint.
The existing provider Test connection also directs operators to this status. Sync retries known journal
artifacts; it never inventories/adopts arbitrary `pcseed-*` files on a shared store.

- **Missing reader/permission:** leave CDs in place, fix the explicit reader's read-only scope, then sync.
- **Unknown mutation:** inspect the exact PVE task/actual+pending configs on the original cluster and all
  references. After proving every matching task finished and original ownership/backing unchanged, an
  operator may reconcile that artifact's `mutationPending` and status in the DB under the seed lock.
  There is intentionally no automatic age-based escape or force-delete API.
- **Stuck operation claim:** pause competing seed operations, prove the original process and all its PVE
  tasks have ended, then reconcile the exact `pveSeedOperation` row under the seed lock. Never clear a live
  claim or auto-expire it. Recovery pins similarly require proving **all provider archives and restore
  tasks** no longer reference any CD; deleting one visible backup is insufficient. No force-clear API.
- **Legacy files:** inventory contents/references with a trusted operator, then act on an exact named file
  only. The journal cannot retrospectively prove whose six-character legacy name it was. Do not bulk
  delete, trust only this portal DB, use VMID absence, or touch another portal's CD.
- **Empty stub:** means payload removed, not physical file removed. Deletion needs optional write-token
  `Datastore.Allocate` on that seed store alone, preserving existing child roles. No global grant.
- **DB restore:** restore identity/journal/source together. Restored state must be reconciled with current
  PVE; a previous grace or terminal state is not permission to recreate/reuse the same filename.

## Deferred, deliberately

No boot-time CD scrub, guest `cloud-init clean`, guest-file deletion, forced reboot, migration change or
metadata URL conversion. A guest agent authenticates transport but tenant root can fake cloud-init done;
arbitrary cloud-init boothooks/per-boot scripts have semantics beyond first boot. A future trusted-image
opt-in must measure same-iid metadata/network-only replacement, keep encrypted source for explicit rebuild,
and verify reboot/NIC/migration/rebuild behavior. CD sanitation cannot erase guest caches or backups.

Acceptance is separate from deployment: hermetic tests cover owned fallback, commit-before-upload,
crash/unknown outcomes, concurrent writers, foreign references, current/pending/snapshot storage aliases,
empty filtered/NoAccess/403/migration failures. A lab drill must use one **fresh** ordinary VM with nonsecret
sentinel/run-once counter and shared store; no existing stack, export or ACL changes. Prove static boot,
reboots, public-NIC reseed, shared migration, rebuild and delete; compare global references/artifacts
before and after. Existing unrelated and legacy CDs must remain untouched. No live drill is claimed here.
