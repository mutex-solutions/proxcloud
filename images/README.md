# OS templates

`images/build.py` turns a vendor cloud image into a template the portal can clone. The installer runs it from the first management host (`installer/playbooks/20-templates.yml`), and `.github/workflows/vm-templates.yml` runs it on a self-hosted runner on a schedule. Both use the `images@pve!packer` token (role `ProxCloudImages`, see [docs/proxmox-permissions.md](../docs/proxmox-permissions.md)).

```text
build.py build --os debian-13 --node pve1 --storage nas-templates --import-storage nas-templates \
               --bridge vmbr0 --build-ip 192.0.2.40/24 --build-gateway 192.0.2.1
```

| Step | What happens |
|---|---|
| Base | The vendor image is downloaded to the store with `import` content, once per upstream checksum. A temporary base template is created from it with a cloud-init drive, a serial console and the agent flag. |
| Packer | `packer/template.pkr.hcl` clones the base, runs `scripts/base.sh` and `scripts/cleanup.sh`, and converts the clone to a template. |
| Smoke test | A clone boots, and the guest agent must report the address cloud-init configured. The clone is then destroyed. |
| Publish | The template gets its public tags. Until then the portal cannot see it. |
| Prune | Versions past `--keep` (default 2) on that node are destroyed. Customer VMs are full clones, so nothing references them. A template without a `pc-v-*` tag is never pruned. |

## Template contract

- `qemu-guest-agent` installed and enabled (the sync reads VM addresses from it)
- a cloud-init drive, and an image that logs in with `sshkeys` or `cipassword` alone
- a scsi boot disk that can be resized
- clean state: cloud-init, machine-id, SSH host keys, logs and SSH keys all cleared

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

Vendor images ship no guest agent, so the build cannot discover a DHCP address. `--build-ip` is a free address on the bridge, used by one build at a time. The machine running `build.py` must reach it over SSH, and Packer needs internet access for its plugin and the VM for `apt`.

## Runner

The workflow needs a repo-scoped self-hosted runner labelled `proxcloud-pve` inside the Proxmox network, with `python3` and `packer` installed. Store **all cluster configuration in repository Actions secrets**, not Variables (Settings → Secrets and variables → Actions → Secrets):

| Kind | Name | Required by / value |
|---|---|---|
| Secret | `PVE_TOKEN_SECRET` | Both workflows: secret of `images@pve!packer` |
| Secret | `PVE_URL`, `PVE_TOKEN_ID` | Both: HTTPS origin + port **without `/api2/json`**; `images@pve!packer` |
| Secret | `PVE_NODES` | Both: nodes that get a copy, space-separated |
| Secret | `PVE_TEMPLATE_STORAGE`, `PVE_BRIDGE` | Both: template storage and build bridge; Kubernetes also needs `iso` content on that storage |
| Secret | `PVE_IMPORT_STORAGE` | OS: storage allowing `import` content for the vendor cloud image |
| Secret | `BUILD_IP`, `BUILD_GATEWAY` | OS: dedicated unused build-VM CIDR (not the runner IP) and gateway |
| Secret | `BUILD_NAMESERVER` | OS, optional: DNS server; empty/unset falls back to `BUILD_GATEWAY` |
| Secret | `BUILD_DHCP_RANGE` | Kubernetes, optional: temporary DHCP start,end pair; also requires `BUILD_GATEWAY` |

See the placeholder-only [.github/pve-actions.env.example](../.github/pve-actions.env.example) for setup instructions and network requirements. Required configuration is checked before checkout/build, reporting missing key names without their values. Both workflows share the `vm-templates` concurrency group so builds remain serialized even with multiple runners.

Actions secrets give best-effort exact-value log masking, not complete confidentiality: transformed values may still appear, and **artifact contents are not masked**. The workflows additionally mask individual node names (and the OS build IP without its CIDR); the OS manifest artifact is allowlisted to `os`, `built`, and `version`, excluding node/VMID/prune details. Review third-party build output before relying on public logs.

The OS workflow runs on dispatch, weekly and on pushes to `main` that touch its image sources; Kubernetes runs on dispatch with a required `version` input. Neither may run on pull request or fork events, because the runner sits inside the Proxmox network and holds the token. Do not assign ordinary public-PR CI to these privileged runners; labels are not a security boundary.

## Kubernetes node templates

`images/k8s-node-template.sh` builds them with [image-builder](https://github.com/kubernetes-sigs/image-builder) at a pinned tag (`.github/workflows/k8s-node-templates.yml` runs it on the same runner, on dispatch), then clones the result to each node in `PVE_NODES`. Its last line is what the kamaji provider takes as the version's image: `1.35.10=pve1=9210,pve2=9211,pve3=9212`.

The Ubuntu installer needs DHCP on the bridge. If the bridge has none, the Actions secrets `BUILD_DHCP_RANGE` and `BUILD_GATEWAY` map to script environment `DHCP_RANGE` and `DHCP_GATEWAY` and run a dnsmasq on the runner's default-route interface. **Only enable this when that interface is on the same L2 network/VLAN as the build bridge, with no competing DHCP server or overlapping pool.** Routed API/SSH reachability alone is not enough; every eligible runner must meet the L2 requirement. This mode also needs sudo/systemd-run/dnsmasq privileges; runners without sudo must use existing DHCP and leave `BUILD_DHCP_RANGE` unset. A build takes about an hour on nested nodes, most of it package pinning. The templates are tagged `pc-k8s`, without `pc-public`, so tenants do not see them.
