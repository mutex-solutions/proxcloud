# Template OS baseline and acceptance gates

This separates two independent image paths. **Source support is not runtime acceptance**; a successful build alone is not an accepted worker image. Nothing in the OS selector automatically upgrades existing templates, providers, or Kubernetes workers.

## Support matrix

| OS | Native OS VM cloud-image builder | Kubernetes Proxmox image-builder wrapper | Runtime evidence before this change |
|---|---|---|---|
| Debian 13 (`debian-13`, trixie) | Existing option | Not supported: no reviewed upstream Proxmox Debian target/preseed | OS build, guest-agent smoke, network cleanup on a development PVE 9.2 host, 2026-11-10 |
| Debian 12 (`debian-12`, bookworm) | Added option; official genericcloud amd64, SHA512 | Not supported; Debian-family Ansible tasks do not constitute a Proxmox Debian build target | New OS canary required |
| Ubuntu 24.04 (`ubuntu-2404`, noble) | Existing option | Retained explicit option; reviewed BIOS target and 24.04 autoinstall seed | OS canary passed as above; Kubernetes static ISO/reboot/Goss acceptance still required |
| Ubuntu 26.04 (`ubuntu-2604`, resolute) | Added option; reviewed dated official amd64 cloudimg, SHA256 | New-build default; reviewed BIOS target and 26.04 autoinstall seed | New OS and Kubernetes canaries required |

The [native OS builder](<../images/build.py>) and [user-only Packer variable files](<../images/packer/os/>) clone official cloud images. The [Kubernetes wrapper](<../images/k8s-node-template.sh>) instead boots a live-server installer ISO and uses the pinned upstream firstboot/reboot/node/Goss provisioners. No broad upstream Ansible port or Debian preseed has been synthesized. EFI metadata exists upstream but is not an exposed/accepted wrapper selector here.

## Reviewed source baseline

Research captured 2026-11-11 identified [image-builder v0.1.55](<https://github.com/kubernetes-sigs/image-builder/releases/tag/v0.1.55>) as the latest stable release then, peeled to immutable commit `7ffb9b7f1f26cd66891874463cc9411e3633325f`. This is a dated research fact, not a claim that it remains latest forever. Both the checkout HEAD and the peeled tag must match before adaptation; caller-provided ref/SHA values cannot redefine the reviewed pin. One fetch origin URL must exactly match `https://github.com/kubernetes-sigs/image-builder` (optional `.git` suffix). Existing dirty/wrong-ref/foreign-or-ambiguous-origin caches and hidden assume-unchanged/skip-worktree flags are refused without in-place checkout/fetch/reset/clean. Use a new `WORK` after operator review. Absent caches are cloned privately. The build tree is then exported from the fixed commit's object bytes, not from working-tree files, with blob-hash/path checks and no copied `.git` configuration/hooks. Replacement refs, fsmonitor and optional locks are disabled for reads, and Make receives an explicit pinned `IB_VERSION`. This keeps a hidden or concurrently edited working-tree Makefile/Ansible file out of execution even if status is misleading.

Official native OS inputs are:

- Debian 12: [bookworm latest genericcloud amd64 QCOW2](<https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2>), [SHA512SUMS](<https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS>), user `debian`.
- Debian 13: [trixie latest genericcloud amd64 QCOW2](<https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2>), [SHA512SUMS](<https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS>), user `debian` (unchanged).
- Ubuntu 24.04: [noble current amd64 cloudimg](<https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img>), [SHA256SUMS](<https://cloud-images.ubuntu.com/noble/current/SHA256SUMS>), user `ubuntu` (unchanged).
- Ubuntu 26.04: [resolute release-20261016 amd64 cloudimg](<https://cloud-images.ubuntu.com/releases/resolute/release-20261016/ubuntu-26.04-server-cloudimg-amd64.img>), [SHA256SUMS](<https://cloud-images.ubuntu.com/releases/resolute/release-20261016/SHA256SUMS>), user `ubuntu`. The reviewed date is explicit rather than inventing a rolling current alias. Updating this source date requires review.

Kubernetes `K8S_OS` accepts exactly `ubuntu-2404` or `ubuntu-2604`. It binds make target, metadata, seed directory, template name, ISO and OS tag together. The upstream [26.04 BIOS metadata](<https://github.com/kubernetes-sigs/image-builder/blob/7ffb9b7f1f26cd66891874463cc9411e3633325f/images/capi/packer/proxmox/ubuntu-2604.json>) names the GA ISO, while this wrapper explicitly overrides its URL and checksum to the [official 26.04.1 live-server ISO](<https://releases.ubuntu.com/26.04/ubuntu-26.04.1-live-server-amd64.iso>) and [official SHA256SUMS](<https://releases.ubuntu.com/26.04/SHA256SUMS>): `cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927`. Ubuntu 24.04 retains the existing 24.04.4 ISO checksum. Complete OS-bound mirrored/custom ISO overrides remain possible; supply and review all three of `ISO_NAME`, `ISO_URL`, `ISO_SHA256` together.

[2404 metadata fixture](<../images/tests/fixtures/image-builder-v0.1.55/ubuntu-2404.json>), [2604 metadata fixture](<../images/tests/fixtures/image-builder-v0.1.55/ubuntu-2604.json>) and [shared BIOS seed fixture](<../images/tests/fixtures/image-builder-v0.1.55/ubuntu-bios-user-data.tmpl>) are verbatim reviewed snapshots, not replacements installed on a live node. The two upstream BIOS seeds are byte-identical at the reviewed commit; preparation nevertheless requires the exact selected upstream directory and refuses a missing/mismatched one. The [preparation tests](<../images/tests/test_k8s_static_config.py>) record their SHA256 identities.

## Bounded defaults and publication semantics

- The [OS workflow](<../.github/workflows/vm-templates.yml>) manual selector defaults to **one OS, `debian-13`**. Options are `debian-13`, `debian-12`, `ubuntu-2404`, `ubuntu-2604`, `all`. Only explicit `all` expands to four.
- Schedule/push events and legacy blank manual input retain the old pair, `debian-13 ubuntu-2404`, not a new four-build burst. All OS/node loops remain serial; both image workflows share `vm-templates` concurrency.
- The [installer defaults](<../installer/playbooks/group_vars/all/defaults.yml>) remain the old two native OS options. An operator may opt into another slug without changing default platform installation behavior.
- The [Kubernetes workflow](<../.github/workflows/k8s-node-templates.yml>) and direct wrapper default `K8S_OS` to **`ubuntu-2604` for new builds**; `ubuntu-2404` remains explicit. The workflow's network default remains **static**; the direct script's legacy network default remains DHCP, so specify static for no-DHCP sites.
- OS tags `pc-public`, `pc-img-<slug>`, `pc-v-<UTC>` retain their existing discovery/pruning behavior. Kubernetes names include OS (`<os>-kube-v<version>`), with `pc-k8s`, the unchanged `pc-k8s-<major>-<minor>-<patch>` version tag, and additional `pc-k8s-os-<os>` tag.
- The Kubernetes output mapping stays `<version>=<node>=<vmid>,...`. Building a different OS does **not** write provider `config.cluster.versions`, stage-50 templates, or a worker upgrade. Existing explicit provider VMID pins remain the operator's choice. Avoid interpreting two OS templates carrying the same version tag as an automatic OS preference.

## Required canary acceptance (operator-run, one OS at a time)

These are follow-up gates, **not actions performed by this source-only implementation**. Use the authorized development lab, reviewed scoped image token, one reserved static build IP/VLAN, correct DNS/egress, and existing serialized workflow controls. No CI dispatch, PVE operation, tag/publication or commit is implied by this document.

1. Review exact selected OS, Kubernetes patch version, ISO/cloud-image URL and checksum, clean pinned checkout, permissions and ISO readiness. The wrapper's inherited asynchronous boot-ISO download/error handling is not proof of completed download; verify exact expected content/checksum before the runtime canary.
2. Native OS: build only the selected slug, verify provisioning, clone/boot guest-agent smoke and usable cloud-init login, then prove no build NIC/identity before public tagging. Do not call a new catalog entry runtime-accepted until this succeeds.
3. Kubernetes static mode: boot the selected installer using the exact local NoCloud seed, not DHCP/seed HTTP; retain upstream firstboot, reboot, node provisioning and Goss tests. Exercise the actual selected OS/Kubernetes pair, not a synthetic fixture.
4. Verify final guest cleanup: no build Netplan/static address, seed/cloud-init cache or installer secrets, temporary builder password/sudo grant/SSH keys, or machine identity. Keep low-privilege runner behavior, private mode0700 run/files0600 and detailed-log redaction intact; never print passwords or export seed artifacts.
5. Prove the exact run-owned uploaded seed ISO detached/deleted and storage absence before **any** template clone/tag. Prove `net0` removed/read back before per-node clone and each template publication. Any cleanup ambiguity fails closed. SIGKILL/cancellation/host loss still needs owner-reviewed exact-resource recovery, not prefix deletion.
6. Only after acceptance, review the produced OS-qualified VMIDs and explicitly choose whether to change a provider version-image pin. Canary build acceptance alone is not an end-to-end cluster provisioning/worker bootstrap or released-platform-install result.

The [builder guide](<../images/README.md>) retains detailed networking, token, storage, cleanup and runtime limitations. Offline [workflow/network regressions](<../images/tests/test_build_network.py>) and [seed/cache regressions](<../images/tests/test_k8s_static_config.py>) add coverage without requiring a live PVE, network or build. They were authored but not executed as part of this source-only task.
