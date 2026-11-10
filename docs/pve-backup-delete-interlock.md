# PVE single-backup deletion: known-active interlock

## Scope and existing authority

The existing authenticated project/MCP `DELETE_BACKUP` action resolves the current
pool VM, lists the provider-selected backup store with `content=backup` and the
server VMID, and accepts only the exact archive volume ID with that project's
notes stamp. Those checks remain before any delete. No new admin endpoint,
credential, storage selection, mutation authority or task engine is introduced.

Single-archive deletion now checks current VM configuration and the existing
active restore-task query before dispatch, matching the bulk path's known-busy
refusal. Any nonempty config `lock` refuses, not just `backup` or `restore`.
A running token-visible `qmrestore` refuses even when the config is unlocked:
a restore may have cut the config down while still reading its input archive.

## C4 component and request sequence

```text
Project/MCP DELETE_BACKUP [existing authentication and project permission]
   -> Service.DeleteServerBackup
      -> resolveOwned: current VM's project pool
      -> ownBackup: provider store / VMID / exact volid / project notes
      -> GetVMConfig: successful current config required
         -> missing/null config or any nonempty lock: REFUSE
      -> RestoreRunning: source=active, vmid, typefilter=qmrestore
         -> read/decode error or missing/null task list: REFUSE
         -> visible active restore: REFUSE
      -> existing deleteArchive
         -> existing holderCheck
         -> exact storage content DELETE
         -> existing WaitTask and backupDenied error classification
```

The only additional shared-helper hardening is in `RestoreRunning`: a successful
HTTP response with missing or `null` data is not an observed empty task list and
now returns a fixed nonsecret error. Explicit JSON `[]` remains valid observed
idle state. The query, token, filters and other task APIs are unchanged. Existing
callers therefore also refuse unavailable task-list envelopes rather than treating
them as idle. Transport/decode errors retain their existing classification; known
lock/restore refusals use the existing conflict contract.

Backup creation, restore pre-stop quota/disk/NIC/extract-config checks, failed
restore recovery, bulk deletion, `RetireServer` ordering, snapshot operations and
holder/seed protocols are not redesigned. The shared HTTP fixture has one optional
request hook with a nil/default path; existing regression bodies and assertions
are unchanged, but the fixture file is not falsely described as byte-identical.

## Deliberate limits

This is **only a known-active/read-error interlock, not an atomic check-to-effect
fence**. A task can appear after the checks. The product token may see only its own
restore tasks; an empty result does not prove that no root/external actor is
running a restore. VMID replacement without a captured incarnation UUID, old issued
requests after a timeout or API crash, alias-provider coordination and external
writers remain separate authority problems.

No new SeedArtifacts wrapper is added. Its session lock/short durable claim and
callback cleanup are not a complete external task fence and must not be sold as
one. No new operation table, lease, UUID adoption, rollback engine or schema is
part of this change. Real PBS/backend failure and restore acceptance have not been
tested here, and no damage or recovery guarantee is inferred from fake wire tests.

## Bounded verification

Hermetic HTTP tests count every outgoing mutation, including unexpected routes:
config locks `backup`, `restore`, migration, arbitrary and whitespace all send
zero content DELETEs; unlocked active restore also sends zero. Config/task
403, 500, malformed JSON, wrong type, missing data and `null` all refuse before
delete. Ownership-negative archive/notes/store/project/pool/VMID paths remain
nonmutating. Successful own-idle deletion makes exactly one correctly encoded
archive DELETE and waits on its UPID; DELETE denial/server failure and task failure
keep their existing errors. Direct helper tests distinguish `[]`, active/ended
lists and unreadable envelopes. Config/list/task counters establish that the tests
reached the relevant guard, rather than passing because the fake omitted a route.

Offline Go 1.25 unit/race verification includes existing backup creation, restore,
bulk/retirement, volume and holder tests without changing their assertions. No
PostgreSQL, container, real PVE/PBS/VM, live API, cloud credential, backup deletion
or restore is needed or authorized by these tests. They do not simulate or claim
that a newly starting restore is fenced after the check.
