# Install ProxCloud

`installer/` brings up the whole platform on a Proxmox VE cluster from one command. Only Proxmox API access is mandatory; everything else has a default and can be overridden.

```sh
cd installer
cp -r inventory/example inventory/mysite
$EDITOR inventory/mysite/hosts.yml inventory/mysite/group_vars/all/proxcloud.yml inventory/mysite/group_vars/all/vault.yml
make preflight INVENTORY=inventory/mysite
make install INVENTORY=inventory/mysite
```

Every stage is idempotent: it reads, then writes only the difference, so re-running `make install` changes nothing on a healthy install.

**Back up `inventory/mysite/generated/`.** It holds the encryption key, database passwords and Proxmox token secrets. The encryption key cannot be recovered, and without it the stored provider secrets are lost. The directory is gitignored.

## What you need

| | |
|---|---|
| Installer host | Linux with `python3`, `openssl`, `docker` and `helm`; `make deps` creates a virtualenv with `ansible-core`. SSH to the management host(s). `docker` and `helm` are for `images.build`, the default. |
| Proxmox VE | 9.2 or newer, datacenter firewall on, nftables firewall on every node. For EVPN tenant networks, `frr-pythontools` on every node. |
| Management host | `mgmt.placement: ssh` (default): 1 or 3 Linux hosts with sudo, 4 vCPU and 8 GiB RAM or more, that are **not** Proxmox nodes. `mgmt.placement: pve-vm`: the installer creates the VMs. |
| Network | The management host reaches the Proxmox API, port 8006, and the internet (k3s, charts, images). The template build needs one free address on the template bridge. |
| Credential | A root-equivalent Proxmox credential for the install, in the vault. The portal itself runs on a least-privilege token the installer creates ([proxmox-permissions.md](proxmox-permissions.md)). |

## Stages

| Stage | Playbook | What it does |
|---|---|---|
| 00 | `00-preflight.yml` | Version, firewall, FRR, reach, refuses a Proxmox node as management host, detects storage and proposes profiles, pins the API certificate, fio class suggestion. |
| 05 | `05-storage.yml` | With a NAS: creates `nas-templates`, `nas-capacity`, `nas-backup` over NFS. |
| 10 | `10-pve-identity.yml` | Roles, users, privilege-separated tokens, ACLs and pools. |
| 25 | `25-mgmt-vms.yml` | `placement: pve-vm` only: the management VMs. |
| 20 | `20-templates.yml` | OS templates through [images/build.py](../images/README.md), from the first management host. |
| 30 | `30-mgmt-cluster.yml` | k3s, always with `--cluster-init`. |
| 35 | `35-local-images.yml` | `images.build`: builds images and chart from this checkout, into a registry on the management cluster. A tag already there is not rebuilt. |
| 40 | `40-mgmt-platform.yml` | ArgoCD, then Applications with inline values: cert-manager, CloudNativePG, ProxCloud (API, consoles, Keycloak, PostgreSQL, RabbitMQ). The scheduler is on. |
| 45 | `45-object-store.yml` | The object store, see below. |
| 60 | `60-register.yml` | Logins, an Admin-API key, the Proxmox provider, fio suggestions; on the first run `deploy/seed/seed-operator.py` (platform and billing configuration, flavors, public price plan, quota). Prints the URLs. |

The RabbitMQ Cluster Operator is applied directly from its release manifest, because upstream ships no chart. k3s's bundled Traefik and ServiceLB serve the portal on the management host's ports 80 and 443. MetalLB and the services cluster arrive with split mode.

## Options

[`installer/playbooks/group_vars/all/defaults.yml`](../installer/playbooks/group_vars/all/defaults.yml) documents every option; [`versions.yml`](../installer/playbooks/group_vars/all/versions.yml) pins every version. Override them under `proxcloud:` in the site's `group_vars`; the two are deep-merged. The ones most sites touch:

| Option | Default | Choices |
|---|---|---|
| `mgmt.placement` | `ssh` | `ssh`, `pve-vm` |
| `ingress.domain` | `<mgmt ip>.sslip.io` | Your domain; the portal uses `app.`, `admin.`, `api.`, `id.`, `s3.` under it |
| `ingress.tls` | `none` | `none`, `acme` (Let's Encrypt, needs public DNS), or the name of a TLS Secret |
| `images.build` | `true` | Builds images and chart from this checkout and pushes them to a registry on the management cluster. `false` pulls the release pinned in `versions.yml` from `images.registry` (for that, a release must be published) |
| `images.pull_secret` | none | `{username, password}` for a private registry |
| `network.kind` | `evpn` | `evpn`, `simple` (one node), `vlan` |
| `object_store.mode` | `vm` | `vm`, `truenas-app`, `external`, `ceph-rgw` |
| `templates.per_node` | `auto` | `auto`: a copy per node unless the default profile is on shared storage |
| `storage_profiles`, `storage_classes` | detected | Override detection; confirm a fio suggestion |

### Access over plain HTTP

`ingress.tls: none` is for a lab or a management network you trust. A browser has no WebCrypto outside a secure context, so the consoles log in without PKCE and the installer stops requiring it on the Keycloak clients; credentials cross the network in the clear. Use `acme` or a TLS Secret on anything else.

### Placement of the management cluster

Never on a Proxmox node: k3s and `pve-firewall` fight over FORWARD and AppArmor, and evictions read the ZFS ARC as used memory. Preflight refuses it.

```yaml
proxcloud:
  mgmt:
    placement: pve-vm
    vm: {servers: 1, ips: [192.0.2.30/24], gateway: 192.0.2.1}
```

The VMs go into pool `proxcloud-system`, which the platform token cannot touch. Three servers need three Proxmox nodes (etcd members on different nodes).

### Storage classes

Preflight detects each storage's type and proposes profiles: `std` on ZFS, `ceph` on RBD, `capacity` on an NFS store with `images` content. An NFS store that also holds `iso` or `import` content is the template store, not a profile. With hosts in the inventory group `pve`, it also runs fio on a 4 GiB scratch volume per storage and suggests a class (`generated/storage-classes.json`):

| p99 sync-write latency | Class |
|---|---|
| under 1 ms | `performance` |
| 1 to 5 ms | `standard` |
| over 5 ms, or under 2,000 randread IOPS | `capacity` |

This is only a suggestion. It lands as `suggestedClass` on the provider, and the operator confirms it in the admin console or with `storage_classes`. The thresholds are `preflight.fio_*`. A NAS's cache can flatter randread, so latency decides first.

### Object store

| Mode | What happens |
|---|---|
| `vm` | A RustFS VM in `proxcloud-system`, cloned from the Debian template, with an XFS data disk on `nas-capacity` when present (qcow2, so the VM can be snapshotted), else on the default profile's storage. |
| `truenas-app` | The TrueNAS community catalog app on the NAS pool, driven through the TrueNAS REST API (`vault.truenas_api_key`). |
| `external`, `ceph-rgw` | Nothing is deployed. `ceph-rgw` registers `object_store.{endpoint,access_key,secret_key}` as the `ceph-s3` provider. Its key belongs to an RGW admin user with caps `users=*;buckets=*;usage=*`. |

For `vm` and `truenas-app`, the public endpoint is `s3.<domain>` through the management ingress with `/rustfs/admin`, `/minio/admin` and the console blocked. The root credentials are generated, FTP, SFTP and WebDAV are off, and the internal buckets `cnpg-backups` and `etcd-snapshots` get versioning, Object Lock GOVERNANCE and a user limited to the bucket.

Not automated yet: the off-site copy, OTLP metrics, and pointing CloudNativePG and k3s backups at those buckets.

## Day 2

| Command | What it does |
|---|---|
| `make upgrade` | After bumping `versions.yml`: k3s, images, chart, operators. |
| `make templates` | Build new template versions now. Keeps two per OS and node. |
| `make storage-rebench` | Re-run fio. Never changes the class of a live volume. |
| `make add-node NODE=pve4` | Checks the node (version, empty, chrony, latency), drops the QDevice when the member count turns odd, joins, FRR, SDN apply, its template copies, fio, re-detect. Safe to re-run on a member. |
| `make remove-node NODE=pve3` | Refuses while guests remain, destroys the node's template copies; a second run with `ARGS='-e pc_node_is_off=true'` deletes the member. |
| `make mgmt-scale` | After adding servers to the inventory or raising `mgmt.vm.servers`: joins them to etcd. |
| `make rustfs-upgrade VERSION=x.y.z` | Read the release notes and advisories first, and add the release digest to `versions.yml`. Stops RustFS, takes a VM snapshot, installs the pinned binary, verifies. |
| `make rustfs-rollback` | Rolls the VM back to that snapshot, binary and data together. Anything written since is lost. |

`add-node` needs SSH from the node to an existing member (`pvecm add --use_ssh`). On a 2-node cluster it does not set up the QDevice for you: it needs a machine outside the cluster.

## Per-organization SSO: restrict Keycloak's egress

If you enable per-organization SSO, Keycloak calls URLs tenants choose, and a DNS name checked when it was saved can
point at an internal address later. No check in the API can close that. Allow Keycloak egress to DNS, its database and
the public internet on 443 only, and deny the pod, service, node and management networks, the Proxmox, TrueNAS and
database addresses, and `169.254.0.0/16`. A `NetworkPolicy` with `ipBlock` `0.0.0.0/0` and an `except` list does it on
a CNI that enforces policies. The installer does not apply one, since the ranges are specific to the site.

## Credentials

- **Installer's admin key:** `proxcloud-api admin-key create -description installer`, run in the API pod by stage 60, mints an Admin-API SigV4 key directly in PostgreSQL. It needs database access and no login, which is what makes it usable before the first operator exists. Delete it from the admin console (API keys) if you do not want to keep it, and remove `generated/admin-api-key.json`.
- **Operator login:** user `keycloak.operator.username` in realm `proxcloud-admin`, password in `generated/operator-password`. That realm has no self-registration, because every login in it is an administrator.
- **Customers:** realm `proxcloud`, with self-registration on.

## Split mode: managed Kubernetes and databases

`proxcloud.mode: split` adds stage 50: Cluster API and CAPMOX on the mgmt cluster build a kubeadm **services cluster** of PVE VMs, which then runs Kamaji, its own Cluster API, ArgoCD, MetalLB, proxmox-csi and the database operators. Stage 60 registers the Kubernetes and database providers.

1. Build the node templates (`images/k8s-node-template.sh`, see [images/README.md](../images/README.md)).
2. Set `proxcloud.services` in the site's group_vars ([defaults.yml](../installer/playbooks/group_vars/all/defaults.yml) documents it): the network, `templates`, `control_plane`, one `workers` entry per PVE node a zone should exist on, and `kamaji.version_images`. A database with replicas needs a worker per replica, on different nodes.
3. `make install`.

The services subnet must be reachable from tenant networks (it holds the API endpoints and database addresses) and must reach the PVE API. Stage 10 grants what per-cluster tokens need, see [proxmox-permissions.md](proxmox-permissions.md#managed-kubernetes): read that first, since it gives the portal token `Permissions.Modify`.

**What ran where:** `50-services-cluster.yml`'s template and `files/services-platform.sh` ran on a lab through hand-written drivers, against a site already installed in `allinone`. The playbook itself, stage 10's split grants and stage 60's registration have not run end to end on a clean site.
