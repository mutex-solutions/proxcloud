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

Proxmox clones to another node only from shared to shared storage. A site whose VM disks live on local ZFS therefore needs a copy of each template on each node, even when the template disk itself is on NFS. The installer does this by default (`templates.per_node: auto`); in the workflow, list the nodes in the `PVE_NODES` variable. A site on Ceph needs one copy.

## Build network

Vendor images ship no guest agent, so the build cannot discover a DHCP address. `--build-ip` is a free address on the bridge, used by one build at a time. The machine running `build.py` must reach it over SSH, and Packer needs internet access for its plugin and the VM for `apt`.

## Runner

The workflow needs a repo-scoped self-hosted runner labelled `proxcloud-pve` inside the Proxmox network, with `python3` and `packer` installed, and these settings:

| Kind | Name | Value |
|---|---|---|
| Secret | `PVE_TOKEN_SECRET` | secret of `images@pve!packer` |
| Variable | `PVE_URL`, `PVE_TOKEN_ID` | API URL; `images@pve!packer` |
| Variable | `PVE_NODES` | nodes that get a copy, space-separated |
| Variable | `PVE_TEMPLATE_STORAGE`, `PVE_IMPORT_STORAGE`, `PVE_BRIDGE` | as for `build.py` |
| Variable | `BUILD_IP`, `BUILD_GATEWAY`, `BUILD_NAMESERVER` | as for `build.py` |

The workflow runs on dispatch, weekly and on pushes to `main` that touch this directory. It must never run on pull request or fork events, because the runner sits inside the Proxmox network and holds the token.

## Kubernetes node templates

`images/k8s-node-template.sh` builds them with [image-builder](https://github.com/kubernetes-sigs/image-builder) at a pinned tag (`.github/workflows/k8s-node-templates.yml` runs it on the same runner, on dispatch), then clones the result to each node in `PVE_NODES`. Its last line is what the kamaji provider takes as the version's image: `1.35.10=pve1=9210,pve2=9211,pve3=9212`.

The Ubuntu installer needs DHCP on the bridge. If the bridge has none, `DHCP_RANGE` and `DHCP_GATEWAY` run a dnsmasq for the build, on the interface of the default route. A build takes about an hour on nested nodes, most of it package pinning. The templates are tagged `pc-k8s`, without `pc-public`, so tenants do not see them.
