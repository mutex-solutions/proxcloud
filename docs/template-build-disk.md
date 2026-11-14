# Template build disk sizes

## Current sizes (owner decision, 2026-11-14)

| Template | Virtual disk | Single source constant | Before |
| --- | --- | --- | --- |
| Native OS (`debian-12`, `debian-13`, `ubuntu-2404`, `ubuntu-2604`) | **5 GiB** | `DEFAULT_BUILD_DISK_GIB` in the [native builder](<../images/build.py>) | 20 GiB |
| Kubernetes node (`ubuntu-2404`, `ubuntu-2604`) | **8 GiB** | `K8S_BUILD_DISK_GIB` in the [Kubernetes build configuration](<../images/k8s-build-config.py>) | 20 GiB |

Why smaller: a template's disk is the smallest disk anything made from it can have, the size every full clone copies first, and the size the offline sanitizer has to write in full (it zeroes the free space). Measured on the lab on 2026-11-14, the 20 GiB templates held 3.8 to 4.1 GB (Kubernetes node), 1.3 GB (`ubuntu-2604`), 0.9 GB (`ubuntu-2404`) and 0.4 GB (Debian). A server or a node made from a template gets the root disk size that was asked for, and cloud-init grows the root filesystem into it at first boot.

What changes for a user:

- **A root disk below 20 GiB is now what the user gets.** A clone is never smaller than its template, so with a 20 GiB template a request for less (the seeded `s1-small` flavor has 10 GiB) still produced a 20 GiB root disk, and 20 GiB was counted against the project quota. With a 5 GiB template the same request produces a 10 GiB disk and counts 10 GiB. On the server create page the minimum root size of the image drops from 20 to 5 GiB, and the default for a flavor below 20 GiB drops to the flavor's own size.
- **A site with copies of both sizes keeps the larger minimum.** Templates are per node, and the installer builds with `--if-missing`, so a node can still hold a 20 GiB copy next to a 5 GiB copy of the same template on another node. The image's minimum disk is the largest copy on any node (`PVEImageCatalog`), because the node is chosen after the size was checked and counted. The minimum drops to 5 GiB when the last 20 GiB copy of that template is gone.

What stays valid:

- **Existing 20 GiB templates** are not rebuilt, resized or withdrawn. The sanitizer and the export tools accept an explicit allowlist per variant: native OS **5 GiB or 20 GiB**, Kubernetes node **8 GiB or 20 GiB** (`VIRTUAL_BYTES` in the [sanitizer](<../images/vm_sanitize.py>)). Any other size is refused; the facts and the manifest carry the size of the disk that was read.
- **Published 20 GiB images** stay valid in the [artifact contract](<public-vm-artifacts.md>), whose virtual size range is now 5..100 GiB for both variants. The contract is deliberately wider than the sanitizer's allowlist: a bound in a released portal cannot be changed afterwards (see the next point), so the exact sizes are enforced where an image is made, not where it is imported.
- **Compatibility:** portals up to **v0.5.2** refuse an image whose virtual disk is below 20 GiB: they read its `manifest.json` during discovery, reject it as an invalid artifact contract and never download the disk. An image built with the new sizes needs a portal that has this change. A portal with this change imports both sizes.
- **Old portals lose a line after its third small image.** A portal up to v0.5.2 checks only the newest three tags of a line (an OS, or an OS with a Kubernetes minor) and offers the first valid one. After one or two small images it lists them as skipped and still offers the newest 20 GiB image. After the third, all three checked tags are small, so it offers nothing on that line; the 20 GiB image is then importable only by an API call that names its digest. Nothing can be changed in the released portals. **Rule: upgrade every portal that follows a line before the third 5 or 8 GiB image is published on it.**

The native builder still only grows: a vendor disk larger than 5 GiB is left as it is, and the sanitizer then refuses that template because its size is not on the allowlist. Add the size deliberately; do not round or shrink.

### What must fail loudly

5 GiB is 1.5 to 2 GiB more than the vendor disks (Debian 3 GiB, Ubuntu 3.5 GiB) on which the Ubuntu 26.04 dist-upgrade ran out of space (see the history below), and 8 GiB is about twice what a built node holds. Neither was proven by a lab build when this was written, so a full disk must stop the build and never produce a template:

- Native OS: both provisioning scripts run under `sh -eu`; a failing `apt-get` ends the Packer build, which ends `build.py` before the smoke test and before the `pc-public` tag. [`base.sh`](<../images/packer/scripts/base.sh>) prints `df -Pk /` before and after the upgrade into the build log.
- Native OS and Kubernetes node: the last cleanup step ([`cleanup.sh`](<../images/packer/scripts/cleanup.sh>), [`k8s-build-cleanup.sh`](<../images/k8s-build-cleanup.sh>)) refuses a root filesystem with less than **1 GiB free** (`MIN_FREE_KIB`). A nearly full image is not converted to a template. The Kubernetes cleanup script runs only in the static network mode; a DHCP build (the default of `k8s-node-template.sh`) keeps image-builder's own last step, so the [build configuration](<../images/k8s-build-config.py>) runs the same four lines as a step of their own just before it.
- Kubernetes node: image-builder's own Ansible run (packages, containerd, `kubeadm config images pull`), reboot and goss stay unchanged and each ends the build on an error. The pinned autoinstall partitions the whole disk as one ext4 root with no swap (`size: -1`), so it has no minimum of its own.

[`test_template_disk_sizes.py`](<../images/tests/test_template_disk_sizes.py>) keeps the constants equal across the builders, the sanitizer, the copy policy, the artifact contract and the portal, and pins these checks.

### First lab build of each image type must confirm

Nothing below was run for this change. Until it is, the new sizes are unproven.

Native OS, once per OS slug (`ubuntu-2604` first: it is the one that failed at 3.5 GiB):

1. The base readback before the first boot is exactly `size=5G`.
2. In the build log, the first `df -Pk /` shows the root filesystem grown to the 5 GiB disk (growpart ran), and the second one shows the room left after the dist-upgrade. Record both numbers.
3. `apt-get dist-upgrade` and the package install end with exit 0, and the cleanup admission passes (1 GiB free or more).
4. The build's smoke clone boots and the template is published at `size=5G`.
5. A server created from it with a larger root disk (for example 10 GiB) boots with the root filesystem grown to that size.
6. The export path: guarded copy with `--expected-bytes 5368709120`, sanitize, postscan and cold boot accept it, and the facts state `virtual_size_bytes: 5368709120`.
7. A portal with this change imports the image and its smoke test passes; a portal at v0.5.2 lists it as skipped and still offers the newest 20 GiB image of that line.
8. On a cluster where another node still has the 20 GiB copy, the image's minimum disk stays 20 GiB; with only 5 GiB copies it is 5 GiB, and an `s1-small` server gets a 10 GiB root disk that counts 10 GiB in the quota.

Kubernetes node, once per OS slug and for the newest Kubernetes version:

1. The template's `scsi0` is `size=8G`.
2. The Ubuntu installer accepts the 8 GiB disk and the image-builder run ends without an out-of-space error in the package install, the containerd install or the kubeadm image pull.
3. The cleanup's `df -Pk /` line: record the used and free space; the admission passes. A DHCP build was not run in CI before: if one is used, its build log must show the same `df -Pk /` line from the added step, before the shutdown.
4. A managed cluster node group with the default 30 GiB root volume joins, and its root filesystem is 30 GiB.
5. The export path: guarded copy with `--expected-bytes 8589934592`, inventory, review against the committed baseline (which records a 20 GiB template; the size is checked against the allowlist instead), sanitize, postscan and cold boot (import read back as `size=8G`, then grown to 30G).
6. A portal with this change imports the image and its smoke test passes.

If a build runs out of space, raise the one constant, add the new size to `VIRTUAL_BYTES` (and the copy policy, the stream gate and the tests that pair them), and build again. Do not suppress the error, skip the upgrade or clean caches to make it fit.

## History: the 20 GiB floor (PR #203)

The sections below describe the change that introduced the floor, at its original value of 20 GiB. The mechanism is unchanged; only the number is now 5 GiB (`size=5G`, 5,368,709,120 bytes).

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
