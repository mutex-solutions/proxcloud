# Native OS template build disk floor

## Failure and scope

The operator-reported Ubuntu 26.04 native cloud-image canary booted and reached SSH, then failed APT dist-upgrade with exit 100 because the guest had insufficient space in `/var/cache/apt/archives`. This is a small vendor QCOW2 virtual-root-disk problem, not evidence that the PVE backing store is full. Boot/SSH success is not template acceptance, and that failed run published no new artifact.

The correction is source-only: the [native builder](<../images/build.py>) enlarges **its own newly created base** before template conversion and before Packer's first boot/provisioning. It is not an existing tenant/server disk resize, a new IaaS default, a provider-pin change, or a Kubernetes installer-ISO sizing change. No tests, PVE calls, network access, CI dispatches, builds, tags, or commits were performed to validate this change.

## Artifact contract

`DEFAULT_BUILD_DISK_GIB = 20` applies to fresh native cloud-image bases for all four supported slugs: `debian-12`, `debian-13`, `ubuntu-2404`, `ubuntu-2604`. This intentionally uses a source constant rather than adding an unreviewed CLI/workflow input. Source URLs, checksums, users, names, versions, Packer/provisioning order, smoke test, build network cleanup and permissions are unchanged.

1. Complete the empty-VM create task for the chosen unused VMID in pool `pc-templates`, then complete the existing `scsi0` import/config task. A failed/unconfirmed create does not authorize mutation or deletion of that ID.
2. Read exact pool membership (including node/type), the node-scoped configuration and stopped status. Require the expected fresh-base name/tags, non-template state and no lock. Do not adopt an older base by name and resize it.
3. Parse exactly one `scsi0` `size=` field into bytes: bare bytes, binary K/M/G/T, or exactly representable decimal values. Reject missing, duplicate, malformed or non-positive sizes. Compare bytes, not rounded GiB, with **21,474,836,480 bytes (20 GiB)**.
4. Only if smaller, issue `PUT /nodes/{node}/qemu/{vmid}/resize` with `disk=scsi0`, **absolute** `size=20G`, and the read configuration's `digest`. No relative `+20G`, no shrinking, no `skiplock`, no alternate/root actor, no ACL expansion. If the upstream disk is already 20 GiB or larger, leave it unchanged.
5. A null/empty synchronous response is not a task: do not call `wait(None)`. A non-empty response must be a well-formed UPID for this node/VMID and must complete successfully. Then recheck ownership/stopped/non-template state and read back the same disk volume at no less than both the floor and original size. Even synchronous success is not sufficient without readback.
6. Only after those checks convert the base to a template and pass it to the unchanged Packer clone path. Cloud-init's existing first-boot growpart/filesystem behavior must make the added virtual capacity usable inside the guest before normal package provisioning.

The API method and absolute size contract are consistent with the existing provider [ResizeDisk implementation](<../internal/cloud/pve/qemu.go#L181-L189>); its [task waiter](<../internal/cloud/pve/task.go#L48-L58>) also treats an absent task ID as synchronous completion. These are local source references, not a new live API probe.

The floor is virtual block capacity, **not** a guarantee of 20 GiB free space, a partition/filesystem change made offline, or a reserved amount of physical storage. PVE allocation/resize errors remain fatal for image creation. Downstream requested disks below the artifact size cannot shrink it; inspect the admitted artifact and flavor expectations before selecting it. Larger IaaS disks still rely on the existing clone-grow and guest cloud-init behavior; this change does not alter product resizing.

## Failure cleanup and preservation

If import, sizing, readback, or conversion fails after a confirmed create, cleanup rechecks the same exact new VMID/node/pool, permitted fresh identity, stopped state, non-template flag and absence of a lock before destroying it. It does not search for a new deletion prefix, stop a running VM, unlock anything, delete a converted/old template, or delete an unconfirmed create/race winner. If that proof or deletion fails, the original failure is preserved and an exact-ID cleanup warning is emitted for operator review. Cancellation, host loss and unconfirmed create tasks still require deliberate recovery; no automatic broad cleanup is implied.

The existing `base` command returns an already matching template **without resizing it**. Existing templates and tenant/system VMs are not retroactively grown. The builder's pre-existing failed-run cleanup and normal post-acceptance `--keep` pruning policy are unchanged; this patch introduces no additional prefix cleanup or old-template deletion. Operators must preserve known-good templates and exact unrelated IDs while reviewing a failed canary.

## Review and acceptance gates (not run here)

- Review this bounded patch and [FakePVE disk tests](<../images/tests/test_build_disk.py>) plus the adapted existing [network regressions](<../images/tests/test_build_network.py>). Unit coverage models all four slugs, byte units/fractional sizes, no-op for larger disks, null/UPID handling, pre-template/pre-Packer readback, failure/no-publication, exact-ID recovery, identity/lock/power guards and untouched existing-base reuse. Tests were **authored, not executed**.
- Only with separate approval, run the offline suite, review its results, then select one fresh native OS canary with the existing scoped image token, reserved build IP/VLAN/DNS and serialization gates. No automatic retry/dispatch is authorized by this document.
- Capture actual pre-first-boot `scsi0` readback at least 20 GiB and a new run-owned base/build identity. Verify no old tenant/system VM, accepted template or provider version pin changed.
- Inside the fresh guest, verify root block device, root partition and filesystem usable capacity before APT. A 20 GiB PVE disk with an unchanged tiny root filesystem fails admission and needs diagnosis, not a bypass. Preserve normal cloud-init behavior/error policy; do not suppress cloud-init errors or substitute repo/cache workarounds.
- Require the unchanged APT update/dist-upgrade, provisioning, guest-agent boot smoke, build-identity cleanup and build-NIC removal/readback gates to pass before public tags. Boot/SSH success or successful resize alone is insufficient. Keep system security checks and failure propagation intact.
- Review exact run-owned cleanup and retained known-good templates afterward. Only a fully successful, separately reviewed canary can count as runtime acceptance; the present patch is not evidence of that result.

See the [builder guide](<../images/README.md>) and [OS baseline/acceptance gates](<template-os-baseline.md>) for the unchanged operating boundary.
