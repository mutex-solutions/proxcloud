# OS templates

`images/build.py` turns a vendor cloud image into a template the portal can clone. The installer runs it from the first management host (`installer/playbooks/20-templates.yml`), and `.github/workflows/vm-templates.yml` runs it on a self-hosted runner on a schedule. Both use the dedicated `images@pve!packer` token; the installer grants `ProxCloudImages`, or an operator can provision equivalent scoped rights ([docs/proxmox-permissions.md](../docs/proxmox-permissions.md)). Installer inventory settings and Actions Secrets are separate configuration paths, not automatically synchronized.

**Validation boundary:** Debian 13 and Ubuntu 24.04 OS cloud-image canaries completed build, guest-agent smoke and cleanup on a development PVE 9.2 host on 2026-11-10 with static IP/public DNS and no DHCP. Debian 12 and Ubuntu 26.04 are additional source-supported OS options; their canaries and both Kubernetes OS/static ISO build/reboot/Goss paths still require runtime acceptance. These changes do not publish portable QCOW2 downloads or prove a clean platform install from released image/chart artifacts. See the [OS baseline and acceptance gates](<../docs/template-os-baseline.md>).

## OS choices and safe defaults

| Catalog slug | Official source / checksum family | Cloud-image login |
|---|---|---|
| `debian-13` | [Debian trixie latest genericcloud amd64](<https://cloud.debian.org/images/cloud/trixie/latest/>), SHA512SUMS | `debian` |
| `debian-12` | [Debian bookworm latest genericcloud amd64](<https://cloud.debian.org/images/cloud/bookworm/latest/>), SHA512SUMS | `debian` |
| `ubuntu-2404` | [Ubuntu noble current amd64 cloudimg](<https://cloud-images.ubuntu.com/noble/current/>), SHA256SUMS | `ubuntu` |
| `ubuntu-2604` | [Ubuntu resolute release-20261016 amd64 cloudimg](<https://cloud-images.ubuntu.com/releases/resolute/release-20261016/>), SHA256SUMS | `ubuntu` |

Ubuntu 26.04 uses the reviewed dated official cloud-image release, **not an invented rolling `resolute/current` URL**. A new source date needs review. Vendor checksum files are read at build time; a checksum change changes the cached import name. The native [OS builder](<build.py>) is independent of Kubernetes image-builder: Debian OS support does not imply a Debian Kubernetes target.

The manual [OS workflow](<../.github/workflows/vm-templates.yml>) offers one of these four slugs or explicit `all`, defaulting to **`debian-13` only** for a bounded canary. Schedule/push events and legacy blank manual input retain **only `debian-13 ubuntu-2404`**. Only `all` selects all four; OS/node loops are serial, sharing one build IP and the cross-workflow concurrency group. Prefer one OS dispatch at a time for canaries. The [installer defaults](<../installer/playbooks/group_vars/all/defaults.yml>) remain `[debian-13, ubuntu-2404]`; adding catalog choices does not expand an existing installation's build list.

```text
build.py build --os debian-13 --node pve1 --storage nas-templates --import-storage nas-templates \
               --bridge vmbr0 --build-ip 192.0.2.40/24 --build-gateway 192.0.2.1
```

| Step | What happens |
|---|---|
| Base | The vendor image is downloaded to the store with `import` content, once per upstream checksum. A temporary base template is created from it with a cloud-init drive, a serial console and the agent flag. |
| Packer | `packer/template.pkr.hcl` clones the base, runs `scripts/base.sh` and `scripts/cleanup.sh`, and converts the clone to a template. |
| Smoke test | A clone boots on the same bridge/access VLAN as the build, and the guest agent must report the address cloud-init configured. The clone is then destroyed. |
| Publish | Only after smoke success is the build NIC (`net0`) removed, then public tags applied. Failed cleanup cannot publish; tenant/server create supplies its own NIC. |
| Prune | Versions past `--keep` (default 2) on that node are destroyed. Customer VMs are full clones, so nothing references them. A template without a `pc-v-*` tag is never pruned. |

## Template contract

- `qemu-guest-agent` installed and enabled (the sync reads VM addresses from it)
- a cloud-init drive, and an image that logs in with `sshkeys` or `cipassword` alone
- a scsi boot disk that can be resized
- clean state: cloud-init, machine-id, SSH host keys, logs and SSH keys all cleared
- no build NIC on published OS/Kubernetes templates: server creation and CAPMOX's `NetworkDevices` configuration create the tenant's `net0` before first boot

## Tags

A template is in the catalog when it is in pool `pc-templates` and tagged:

| Tag | Meaning |
|---|---|
| `pc-public` | listed for tenants |
| `pc-img-<slug>` | the catalog entry, e.g. `pc-img-debian-13` |
| `pc-v-<yyyymmddhhmm>` | version in UTC; the portal uses the newest per slug and per node |

Discovery is `pve.DiscoverTemplates`. `config.templates` on the provider is optional: an entry with `vmid` and `node` pins that template, any other entry only overrides `name`, `osDistro` and `minDiskGb` for its slug.

## One copy per node

Proxmox clones to another node only from shared to shared storage. A site whose VM disks live on local ZFS therefore needs a copy of each template on each node, even when the template disk itself is on NFS. The installer does this by default (`templates.per_node: auto`); in the workflow, list the nodes in the `PVE_NODES` Actions secret. A site on Ceph needs one copy.

## Build network

The OS cloud-image path does not rely on DHCP or guest-agent address discovery. `--build-ip` is a reserved unused host CIDR on the build bridge/access VLAN, used by one build at a time; exclude it from DHCP and other allocators and recheck before use. The machine running `build.py` must reach that guest over SSH. Packer needs internet access for plugins, and the guest needs working DNS and package-repository access for `apt`. A DNS response from the runner's network does not prove the guest VLAN is allowed to use the same resolver.

`PVE_BUILD_VLAN` is an optional Actions secret for the **build guest's** access VLAN: omitted/empty means `0` (untagged); decimal `1..4094` tags the build NIC. It does not reconfigure the runner's network. The bridge/uplink must carry the chosen VLAN, and the build VM's address/gateway/DNS (or Kubernetes DHCP) must work on it. Direct OS builds use the matching `--vlan` flag. Both provisioning and the OS smoke clone retain the build VLAN; only after successful tests is `net0` deleted before public tags or Kubernetes per-node cloning. No untagged replacement NIC is attached, so the build token need not be granted access to unrelated native networks.

## Runner

The workflows need a repo-scoped **self-hosted** runner labelled `proxcloud-pve`, with PVE API/build-guest SSH reachability, `python3` and `packer` installed (Kubernetes static mode also needs xorriso/OpenSSL). This runner orchestrates PVE; it does not run the nested build VM locally. GitHub-hosted test/container/chart jobs need no PVE runner. Store **all cluster configuration in repository Actions secrets**, not Variables (Settings → Secrets and variables → Actions → Secrets):

| Kind | Name | Required by / value |
|---|---|---|
| Secret | `PVE_TOKEN_SECRET` | Both workflows: secret of `images@pve!packer` |
| Secret | `PVE_URL`, `PVE_TOKEN_ID` | Both: HTTPS origin + port **without `/api2/json`**; `images@pve!packer` |
| Secret | `PVE_NODES` | Both: nodes that get a copy, space-separated |
| Secret | `PVE_TEMPLATE_STORAGE`, `PVE_BRIDGE` | Both: template disk/cloud-init storage (`images` content) and build bridge |
| Secret | `PVE_BUILD_VLAN` | Both, optional: build-guest access VLAN; default `0` untagged, or decimal `1..4094` |
| Secret | `PVE_ISO_STORAGE` | Kubernetes, optional: `iso`-capable installer storage; defaults to `PVE_TEMPLATE_STORAGE` |
| Secret | `PVE_IMPORT_STORAGE` | OS: storage allowing `import` content for the vendor cloud image |
| Secret | `BUILD_IP`, `BUILD_GATEWAY` | OS and Kubernetes static mode: dedicated unused build-VM CIDR (not the runner IP) and gateway |
| Secret | `BUILD_NAMESERVER` | Kubernetes static mode requires one to three space-separated IPv4 DNS servers; OS may omit/fall back to gateway |
| Secret | `BUILD_DHCP_RANGE` | Kubernetes, optional: temporary DHCP start,end pair; also requires `BUILD_GATEWAY` |

See the placeholder-only [.github/pve-actions.env.example](../.github/pve-actions.env.example) for setup instructions and network requirements. Required configuration is checked before checkout/build, reporting missing key names without their values. Both workflows share the `vm-templates` concurrency group so builds remain serialized even with multiple runners.

Actions secrets give best-effort exact-value log masking, not complete confidentiality: transformed values may still appear, and **artifact contents are not masked**. The workflows additionally mask individual node names (and the OS build IP without its CIDR); the OS manifest artifact is allowlisted to `os`, `built`, and `version`, excluding node/VMID/prune details. Review third-party build output before relying on public logs.

The OS workflow runs on dispatch, weekly and on pushes to `main` that touch its image sources; Kubernetes runs on dispatch with a required `version` input. Neither may run on pull request or fork events, because the runner sits inside the Proxmox network and holds the token. Do not assign ordinary public-PR CI to these privileged runners; labels are not a security boundary.

## Kubernetes node templates

The [Kubernetes builder](<k8s-node-template.sh>) uses [image-builder v0.1.55](<https://github.com/kubernetes-sigs/image-builder/releases/tag/v0.1.55>), verified against peeled immutable commit `7ffb9b7f1f26cd66891874463cc9411e3633325f`. The [manual workflow](<../.github/workflows/k8s-node-templates.yml>) has an `os` choice; direct calls use `K8S_OS`. Both default to **`ubuntu-2604` for new builds**, retaining `ubuntu-2404` explicitly. The reviewed upstream targets/seeds exist for both; Debian 12/13 and arbitrary/EFI selectors are rejected, not mapped onto Ubuntu or an unreviewed Debian preseed. Upstream's additional EFI targets are not exposed by this wrapper.

The selected OS determines `build-proxmox-<os>`, the exact `24.04` or `26.04` upstream seed directory, `<os>-kube-v<K8S_VERSION>` template names, and the checked ISO tuple. Ubuntu 24.04 retains its existing 24.04.4 ISO/SHA; Ubuntu 26.04 selects the [official 26.04.1 live-server amd64 ISO](<https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso>), SHA256 `cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927` from [SHA256SUMS](<https://releases.ubuntu.com/26.04/SHA256SUMS>). The helper passes that ISO URL/checksum into Packer variables too, rather than silently using upstream's older 26.04 GA checksum. Mirrored/custom `ISO_NAME`, `ISO_URL`, `ISO_SHA256` overrides must be supplied together and match the selected Ubuntu release; they remain an operator-reviewed input, not automatic upstream verification.

Published templates retain `pc-k8s` and version tag `pc-k8s-<major>-<minor>-<patch>`, adding `pc-k8s-os-<os>`. The OS-qualified name avoids same-version cross-OS adoption. The last output line stays version-only for the kamaji provider: `1.35.10=pve1=9210,pve2=9211,pve3=9212`. It is an output mapping, **not a provider update**: building another OS never edits existing `config.cluster.versions`, stage-50 templates, or existing workers. Review/accept and explicitly select the desired VMIDs to switch a provider pin.

A present `WORK/image-builder` cache must have one fetch origin URL, exactly `https://github.com/kubernetes-sigs/image-builder` (optional `.git` suffix), with both HEAD and the peeled `v0.1.55` tag at the reviewed commit. Tracked, untracked **or ignored** changes and hidden `assume-unchanged`/`skip-worktree` flags are refused. A mismatch fails before PVE access without fetching/resetting/cleaning the cached repository; choose a new `WORK` after review. If no cache exists, the wrapper clones into a private checkout. Before rendering, either source is reverified and only the **fixed commit's object bytes** are exported into a new private build tree, never copied from the mutable working tree. The export excludes cached `.git` config/hooks, validates blob object hashes and paths, and preserves only internal symlinks/executable modes. Git reads disable replacement refs, fsmonitor and optional index locks; Make receives explicit pinned `IB_VERSION` because the export has no `.git`. `IMAGE_BUILDER_REF`/`IMAGE_BUILDER_COMMIT` environment overrides cannot substitute an unreviewed source pin.

The pinned image-builder `v0.1.55` already exposes its adapter's `vlan_tag` through `PROXMOX_VLAN`; the wrapper passes the validated build VLAN there (empty for untagged) and refuses incompatible upstream template overrides rather than silently ignoring it. In-guest provisioning/reboot/goss checks run on the real build network. After they succeed, the source template's `net0` is deleted and read back before cloning; each existing or new per-node copy is likewise checked before `pc-k8s` publication. CAPMOX adds the tenant's NIC from the chart's explicit `networkDevices` spec before worker bootstrap. Optional `PVE_ISO_STORAGE` separates installer ISO content from template disk storage; the default preserves sites using one image/ISO-capable store.

### Static mode (workflow default; no DHCP)

The manual workflow's `network_mode` defaults to **static**. It consumes the existing `BUILD_IP` host CIDR, `BUILD_GATEWAY`, `BUILD_NAMESERVER` (one to three space-separated IPv4 addresses), and optional build VLAN secrets. The shell entry point alone keeps `PVE_BUILD_NETWORK_MODE=dhcp` as a backward-compatible default; select `static` explicitly for direct invocation. Static mode refuses DHCP_RANGE and validates IP/gateway/DNS/version before PVE mutations.

The static configuration is designed to boot the pinned Ubuntu ISO with a locally attached **CIDATA NoCloud seed ISO**, not a URL fetched using DHCP. Its root `user-data`, unique `meta-data` instance ID, and `network-config` provide fixed-MAC static Netplan for the live installer; the identical mapping under `autoinstall.network` targets the installed OS across the provisioning reboot. The prior 24.04 adaptation had offline/Packer validation; this OS-selector change and its added tests were authored without execution, and neither selected OS is a new live ISO acceptance result. Packer receives an explicit SSH host. No inbound runner HTTP server, seed-host/NAT override, pfSense DHCP change, or host-network change is needed. The runner needs **xorriso, OpenSSL**, and ISO upload/delete rights on the selected ISO storage. Packer comes from the existing executable (`PACKER_BIN`), and the adapted Proxmox plugin is pinned to `1.2.4`.

Upstream Ansible firstboot, reboot, node provisioning and goss tests remain in their original order. Only the final root shutdown provisioner additionally removes installed static Netplan, installer/cloud-init seed/cache/logs, temporary builder password hash/sudo rule/SSH keys, then cleans cloud-init for the tenant's next bootstrap. It never reapplies networking or reboots with the seed still attached. After Packer succeeds, the wrapper verifies the exact run-owned seed ISO is detached, deletes it, waits for any task and proves storage absence **before cloning/tagging**. It then removes the build NIC as above. Static mode refuses adopting existing same-name templates because their guest-disk cleanup cannot be proved by name alone; a rebuild/new version needs operator review.

All rendered credentials, seed files and detailed make/Packer output stay under a per-run mode0700 directory (files0600), not public logs/artifacts. Upstream's password-rendering `tee` target is bypassed. Successful runs remove private state; failures retain private logs/recovery notes when needed. Failure to prove remote seed detachment/deletion refuses publication. **SIGKILL, runner cancellation or host loss is not guaranteed cleanup:** recover only exact owned media/VMs after review; never delete by a loose name prefix. The [preparation helper](<k8s-build-config.py>) supports `check`, `select`, read-only `verify-checkout --checkout <path>`, `export-checkout --checkout <path> --run <private-run>` (writes only private object data), and `prepare --capi <private-run>/image-builder/images/capi --run <private-run>`; it does not contact PVE. Offline regressions in [network/workflow tests](<tests/test_build_network.py>) and [seed/cache tests](<tests/test_k8s_static_config.py>) use stub commands and reviewed upstream OS metadata/seed fixtures for both selectors. Adding these tests is not a claim that they or a live canary have been run. Before the first runtime canary, verify that the boot installer ISO download has completed and the expected checksum/content is present: the wrapper's legacy asynchronous boot-ISO download/error handling is not proof of readiness.

### DHCP mode (explicit legacy option)

`network_mode=dhcp` retains the upstream HTTP autoinstall path, requiring trusted DHCP on the guest bridge/VLAN **and guest-to-runner seed HTTP reachability**. If that bridge has no DHCP, `BUILD_DHCP_RANGE` and `BUILD_GATEWAY` map to script environment `DHCP_RANGE` and `DHCP_GATEWAY` and run dnsmasq on the runner's default-route interface. **Only enable this when that interface is on the same L2 network/VLAN as the build bridge, with no competing DHCP server or overlapping pool.** Routed API/SSH alone is insufficient. This optional mode needs reviewed sudo/systemd-run/dnsmasq privileges; no-sudo runners use static mode or existing DHCP with a working seed HTTP route. A build may take about an hour on nested nodes. Templates are tagged `pc-k8s`, without `pc-public`, so tenants do not see them. Static-mode runtime acceptance must be performed separately after choosing the Kubernetes version; offline tests are not a real ISO/PVE build.
