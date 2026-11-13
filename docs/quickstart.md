# Quick start

**Release prerequisite:** broker-free deployment targets the stable **v0.5.0**
release, not the older Rabbit-dependent API. Its tag publishes the images and app
chart; until you have verified them pullable, use the source installer's default
`images.build: true` or build/load known compatible images for Helm. The image/chart
pin alone is not proof of publication. Public `dev-<sha>` container images are
development artifacts, not a versioned chart. Live installer and split-mode
acceptance have not been performed for this release.

**Validation boundary (2026-11-10):** Debian 13 and Ubuntu 24.04 OS template build,
smoke and build-network cleanup passed on a PVE 9.2 development host using static
addresses. The Kubernetes static installer-seed path has automated tests and full
Packer configuration validation, but no real ISO boot/reboot/Goss acceptance yet.
Neither result establishes a clean split-mode install or production readiness.

Pick the row that matches what you want, then follow its section. Each section has the
inventory keys to fill, the one command, what success looks like and the first steps after.
The long form of the installer (every stage, every option, day 2) is [install.md](install.md).

Addresses and names below are documentation examples (`192.0.2.x`, `198.51.100.x`,
`203.0.113.x`, `example.org`). Replace them with yours.

## 1. Choose your path

| You want to | Path | What gets deployed | You need first | Command |
|---|---|---|---|---|
| Try the consoles on a laptop, no Proxmox | [A](#a-laptop-docker-compose) | API, customer and operator consoles, PostgreSQL (also durable jobs), a dev Keycloak | Docker with Compose | `docker compose up --build` |
| Point that laptop stack at a Proxmox cluster you already have | [A](#a-laptop-docker-compose) + [A2](#a2-connect-an-existing-proxmox-cluster) | The same, plus one Proxmox provider you register by hand | PVE 9.2+, a token made with `pveum`, OS templates | operator console |
| Evaluate on one Proxmox node | [B](#b-one-proxmox-node-evaluation) | A management VM on that node (k3s, ArgoCD, the platform), a RustFS VM, OS templates, the provider | One PVE 9.2+ node, three free addresses | `make install` |
| A small production site, one management host | [C](#c-production-one-management-host-allinone) | k3s on your Linux host, the platform, RustFS, templates, the provider, optional NAS stores | PVE 9.2+ cluster, a Linux host that is not a PVE node, a DNS domain | `make install` |
| Managed Kubernetes, managed databases, sandboxes | [D](#d-managed-kubernetes-databases-and-sandboxes-split) | All of C, plus a services cluster (Kamaji, Cluster API, MetalLB, database operators), optionally a sandbox cluster | Path C installed, k8s node templates, a services subnet | `make install` with `mode: split` |
| The portal only, on a Kubernetes cluster you run | [E](#e-install-on-kubernetes-with-helm) | API, both consoles, PostgreSQL durable jobs and Keycloak (quick start), or CloudNativePG and your OIDC issuer (production), no broker | Any cluster with a default StorageClass (quick start); the CloudNativePG operator and an OIDC issuer (production) | `helm install` |

Then add what you sell, on any installer path:

| Add | Section | Switch |
|---|---|---|
| Standard tier: local ZFS, TrueNAS NFS/SMB/iSCSI, RustFS S3 | [Storage tier](#3-storage-tier-standard-or-full) | `nas.*`, `object_store.mode: vm` or `truenas-app` |
| Full tier: Ceph RBD/CephFS, Ceph RGW S3 | [Storage tier](#3-storage-tier-standard-or-full) | Ceph stores on PVE, `object_store.mode: ceph-rgw` |
| GPUs, DNS zones, load balancers, backups, per-organization SSO, payments | [Add-ons](#4-add-ons) | operator console, mostly |

## 2. Paths

### A. Laptop (Docker Compose)

No Proxmox needed. Cloud pages stay empty until a provider is registered.

1. From the repository root:

   ```sh
   docker compose up --build
   ```

2. Open:

   | What | URL | Login |
   |---|---|---|
   | Customer console | `http://localhost:8082` | `demo` / `demo` |
   | Operator console | `http://localhost:8083` | `admin` / `admin` |
   | API | `http://localhost:8080`, health on `http://localhost:8081/actuator/health` | |
   | Keycloak | `http://localhost:8090` | `admin` / `admin` |

   These are fixed development credentials (`deploy/dev/keycloak-realm.json`, `docker-compose.yml`). Never expose
   this stack. To reach it from another machine, put `PROXCLOUD_HOST=<this machine's address>` in a `.env` file next
   to `docker-compose.yml`.

Scheduler/debug gates default **off**; PostgreSQL workers always finish any
already-enqueued work. No RabbitMQ container runs by default. Existing Compose
users must follow the [cutover runbook](postgres-jobs-migration.md) and preserve
the old broker's anonymous volume; replace only the API, never `down -v`.

More: [development.md](development.md).

### A2. Connect an existing Proxmox cluster

1. On a PVE node, create the roles, the user `proxcloud@pve`, its token `platform` and the ACLs: the
   [pveum commands](proxmox-permissions.md#pveum-commands). Keep the token secret it prints.
2. Turn on the datacenter firewall and the nftables firewall on every node (Datacenter → Firewall → Options, and
   each node → Firewall → Options). Test connection refuses a cluster without them.
3. Templates: tenants clone servers from templates tagged `pc-public` in pool `pc-templates`. Build them with
   [images/build.py](../images/README.md), or use path B, which builds them for you.
4. Operator console → System → Cloud providers → Add provider → Proxmox: API URL `https://192.0.2.11:8006`, token ID
   `proxcloud@pve!platform`, the secret, the certificate fingerprint. Then **Test connection**.
5. Give it the customer basics (flavors, public price plan, billing configuration, quota) in one command:

   ```sh
   PROXCLOUD_HOST=localhost deploy/seed/seed-operator.py --provider "<provider name>"
   ```

### B. One Proxmox node (evaluation)

The installer creates the management VM on the node itself (`mgmt.placement: pve-vm`), with a flat network
(`network.kind: simple`, no FRR) and plain HTTP on `<address>.sslip.io`.

**Before you start**

- Installer host: Linux with `python3`, `openssl`, `docker`, `helm`, internet access, an SSH key in
  `~/.ssh/id_rsa.pub` or `~/.ssh/id_ed25519.pub`, and SSH reach to the addresses below.
- PVE 9.2 or newer. Datacenter firewall on, nftables firewall on the node (step 2 of A2).
- A store with `import` content for the vendor cloud images. The installer takes the first such store other than
  `local`, then `local` itself. Without a NAS, add `import` to `local`'s content types (Datacenter → Storage →
  `local` → Content).
- A store with `images` content for VM disks (`local-zfs` or `local-lvm` on a default install).
- Three free addresses on `vmbr0`: the management VM, the RustFS VM, the template build VM.

**Steps**

1. Copy the example site:

   ```sh
   cd installer
   cp -r inventory/example inventory/lab
   ```

2. `inventory/lab/hosts.yml`: empty the `mgmt` group; the installer fills it.

   ```yaml
   all:
     children:
       mgmt:
         hosts: {}
   ```

3. `inventory/lab/group_vars/all/vault.yml`: a root-equivalent token for the install.

   | Key | Meaning | Example |
   |---|---|---|
   | `proxcloud_vault.pve_token_id` | Install credential (or `pve_user` + `pve_password`) | `root@pam!installer` |
   | `proxcloud_vault.pve_token_secret` | Its secret | `00000000-0000-0000-0000-000000000000` |

4. `inventory/lab/group_vars/all/proxcloud.yml`, replacing the example content:

   ```yaml
   proxcloud:
     pve:
       api_url: https://192.0.2.11:8006
     mgmt:
       placement: pve-vm
       vm: {servers: 1, ips: [192.0.2.30/24], gateway: 192.0.2.1}
     network:
       kind: simple
     templates:
       build_ip: 192.0.2.40/24
       build_gateway: 192.0.2.1
     object_store:
       mode: vm
       vm: {ip: 192.0.2.41/24, gateway: 192.0.2.1}
   ```

   | Key | Meaning |
   |---|---|
   | `pve.api_url` | The Proxmox API as the installer host reaches it. The only mandatory key |
   | `mgmt.placement`, `mgmt.vm.ips`, `mgmt.vm.gateway` | Create one management VM with this address |
   | `network.kind` | `simple`: one SDN zone without EVPN, for a single node |
   | `templates.build_ip`, `templates.build_gateway` | A free address the template build VM uses |
   | `object_store.vm.ip`, `object_store.vm.gateway` | The RustFS VM (S3 for tenants and internal backups) |

   No `network.public` here: on one node the only bridge is usually the management bridge, which the portal refuses
   as a public network. Servers are then reached through the console.

5. Check, then install:

   ```sh
   make preflight INVENTORY=inventory/lab
   make install INVENTORY=inventory/lab
   ```

**Success** looks like the last task printing:

```text
Customer console  http://app.192.0.2.30.sslip.io
Operator console  http://admin.192.0.2.30.sslip.io  login admin, password in .../generated/operator-password
API               http://api.192.0.2.30.sslip.io
Keycloak          http://id.192.0.2.30.sslip.io  admin password in .../generated/keycloak-admin-password
```

`sslip.io` names resolve to the address inside them, so the browser needs public DNS and a route to the management
VM. Then follow [After any install](#5-after-any-install).

### C. Production, one management host (`allinone`)

**Before you start**

- Everything in B's list, except that the management host is a Linux host you provide: 4 vCPU, 8 GiB RAM, sudo,
  **not** a Proxmox node (preflight refuses one), reaching the PVE API on 8006 and the internet.
- With the default EVPN tenant networks: `frr-pythontools` on every node (`apt install frr-pythontools`).
- A DNS domain whose `app.`, `admin.`, `api.`, `id.` and `s3.` names point at the management host, for `tls: acme`.
- Optional: a public bridge on the nodes for tenant public IPs, and a TrueNAS for shared stores.

**Steps**

1. `cp -r inventory/example inventory/mysite`
2. `inventory/mysite/hosts.yml`:

   ```yaml
   all:
     children:
       mgmt:
         hosts:
           mgmt1: {ansible_host: 192.0.2.10, ansible_user: debian}
       pve:
         hosts:
           pve1: {ansible_host: 192.0.2.11, ansible_user: root}  # optional: fio storage benchmark, add-node
   ```

3. `vault.yml` as in B, then encrypt it: `.venv/bin/ansible-vault encrypt inventory/mysite/group_vars/all/vault.yml`
   (after `make deps`), and add `ARGS=--ask-vault-pass` to every `make` call.
4. `proxcloud.yml`:

   ```yaml
   proxcloud:
     pve:
       api_url: https://192.0.2.11:8006
     ingress:
       domain: cloud.example.org
       tls: acme
       acme_email: ops@example.org
     keycloak:
       operator: {username: admin, email: ops@example.org}
     network:
       public:
         id: vmbr1
         name: Internet
         cidr: 203.0.113.0/24
         gateway: 203.0.113.1
         ranges: [203.0.113.10-203.0.113.200]
     templates:
       build_ip: 192.0.2.40/24
       build_gateway: 192.0.2.1
     object_store:
       mode: vm
       vm: {ip: 192.0.2.41/24, gateway: 192.0.2.1}
   ```

   | Key | Default | Meaning |
   |---|---|---|
   | `ingress.domain` | `<mgmt ip>.sslip.io` | Portal hosts `app.`, `admin.`, `api.`, `id.`, `s3.` under it |
   | `ingress.tls` | `none` | `acme` (Let's Encrypt, public DNS), or the name of a TLS Secret. `none` sends logins in clear text |
   | `ingress.acme_email` | empty | Let's Encrypt account email |
   | `keycloak.operator.email` | `admin@example.com` | The operator login; its domain is reserved against customer SSO claims |
   | `network.public` | none | Tenant public IPs: a bridge other than the management bridge, its subnet, gateway and pool |
   | `network.kind` | `evpn` | `evpn` (needs FRR), `simple` (one node), `vlan` (`network.bridge`, `vlan_min`, `vlan_max`) |
   | `network.cidr_pool` | `10.100.0.0/16` | Tenant subnets are carved from it; must not overlap your networks |
   | `provider.quota` | 20 servers, 80 vCPU, 100 GiB RAM, 2000 GB disk | Default quota of each new project |

5. Install:

   ```sh
   make preflight INVENTORY=inventory/mysite ARGS=--ask-vault-pass
   make install INVENTORY=inventory/mysite ARGS=--ask-vault-pass
   ```

Success is the same printout as B, on `https://app.cloud.example.org` and so on.
**Back up `inventory/mysite/generated/`**: it holds the encryption key (unrecoverable), database passwords and
token secrets. Then follow [After any install](#5-after-any-install).

### D. Managed Kubernetes, databases and sandboxes (`split`)

`mode: split` adds a services cluster (stage 50) that runs Kamaji, Cluster API, MetalLB, proxmox-csi and the
database operators, and registers the Kubernetes and database providers (stage 60). Details:
[install.md § Split mode](install.md#split-mode-managed-kubernetes-and-databases), [managed-k8s.md](managed-k8s.md),
[managed-dbaas.md](managed-dbaas.md).

1. Install path C first. It creates the `images@pve!packer` token the next step uses.
2. Build the Kubernetes node templates separately, before stage 50 uses them. Stage 50 consumes template IDs;
   it does **not** invoke the template builder. Choose an offered Kubernetes version deliberately (`1.35.10` below
   is the current BOM example, not a claim that its new static ISO build passed).

   For a direct invocation from the repository root, source your protected local environment or export the
   `PVE_*` credential/configuration first; never commit a populated file or print its contents:

   ```sh
   # Set PVE_URL, PVE_TOKEN_ID, PVE_TOKEN_SECRET and PVE_NODES privately first.
   PVE_BUILD_NETWORK_MODE=static \
   PVE_TEMPLATE_STORAGE=template-disks PVE_ISO_STORAGE=installer-isos PVE_BRIDGE=vmbr0 \
   PVE_BUILD_VLAN=0 BUILD_IP=192.0.2.40/24 BUILD_GATEWAY=192.0.2.1 \
   BUILD_NAMESERVER="192.0.2.53 192.0.2.54" \
   K8S_VERSION=1.35.10 bash images/k8s-node-template.sh
   ```

   `PVE_TEMPLATE_STORAGE` needs `images` content for disks/cloud-init; `PVE_ISO_STORAGE` needs `iso` content for
   the Ubuntu installer and temporary private seed. It defaults to the template store only when that store supports
   both. Set `PVE_BUILD_VLAN` to the guest access VLAN, or `0` for untagged; tagging does not change the runner's NIC.
   Reserve/exclude `BUILD_IP` from DHCP and other allocations, and verify gateway/DNS and runner-to-guest SSH reachability.

   Static mode attaches a local NoCloud `cidata` ISO, sets the network for the live installer and installed build
   system, and passes a fixed SSH host to Packer. It needs **xorriso and OpenSSL** on the build host, scoped ISO
   upload/delete rights and internet access from the guest; it does **not** require DHCP or a guest-to-runner HTTP
   seed route. The manual GitHub workflow defaults `network_mode` to `static`, while a direct shell invocation
   defaults to legacy `dhcp` unless `PVE_BUILD_NETWORK_MODE=static` is explicit. For Actions, put all cluster
   values in **Repository Secrets** using [.github/pve-actions.env.example](../.github/pve-actions.env.example),
   not workflow YAML/Variables. Do not route public-PR jobs to the privileged runner.

   Before publication the builder sanitizes the guest, verifies the exact temporary seed is detached/deleted and
   removes the build NIC. Detailed logs/seed files are private (directory `0700`, files `0600`); cancellation or
   host loss can bypass cleanup and requires owner-checked recovery. Static mode refuses old same-name templates
   without in-guest cleanup proof. Do not enable a competing DHCP server to work around a failed build. If you
   intentionally choose `dhcp`, see its distinct network/HTTP requirements in
   [images/README.md](../images/README.md#dhcp-mode-explicit-legacy-option).

   The successful builder's last line, e.g. `1.35.10=pve1=9210,pve2=9211,pve3=9212`, fills `services.templates` and
   `services.kamaji.version_images` below. Real static-mode ISO boot/reboot/Goss acceptance is still pending;
   validate it on your chosen version before offering the resulting image to tenants.
3. Add to `proxcloud.yml`:

   ```yaml
   proxcloud:
     mode: split
     services:
       templates: {pve1: 9210, pve2: 9211, pve3: 9212}
       control_plane: {node: pve1}
       workers:
         - {node: pve1, cores: 4, memory: 16384, disk: 60}
         - {node: pve2, cores: 4, memory: 16384, disk: 60}
         - {node: pve3, cores: 4, memory: 16384, disk: 60}
       network:
         bridge: vmbr0
         gateway: 192.0.2.1
         endpoint: 192.0.2.200           # the services API (kube-vip)
         nodes: [192.0.2.201-192.0.2.209]
         metallb: 192.0.2.210-192.0.2.229
       kamaji:
         version_images: {"1.35.10": "pve1=9210,pve2=9211,pve3=9212"}
   ```

   | Key | Meaning |
   |---|---|
   | `services.templates` | `{pve node: template vmid}` from step 2; every node named below needs one |
   | `services.control_plane.node`, `services.workers` | Where the services VMs run; one worker per node a zone should exist on, one per database replica |
   | `services.network.*` | A subnet tenants can reach and that reaches the PVE API: API endpoint, node range, MetalLB range |
   | `services.kamaji.version_images` | Kubernetes versions offered to tenants; read once, at the first registration |
   | `services.storage`, `services.iso_storage` | Default `local-zfs` and `local`; the ISO store must be node-local |

   Keep `object_store.mode` at `vm` (the default) or `truenas-app`: database backups go there.

4. `make install INVENTORY=inventory/mysite ARGS=--ask-vault-pass`. The providers `<name> Kubernetes` and
   `<name> Databases` appear under Cloud providers.

**Sandboxes** (gVisor, E2B-compatible API) add `sandbox.*` on top of split. Minimum keys, each explained in
[install.md § Sandbox cluster](install.md#sandbox-cluster-split-mode):

```yaml
proxcloud:
  sandbox:
    enabled: true
    network:
      subnet: 198.51.100.0/24           # outside network.cidr_pool
      gateway: 198.51.100.1
      nodes: 198.51.100.10-198.51.100.29
      metallb: 198.51.100.200-198.51.100.209
      snat: 203.0.113.250               # the sandbox's own egress address, outside network.public.ranges; required ("node" shares the exit node's)
    registry:
      address: 192.0.2.226              # inside services.network.metallb
```

The exit node of the sandbox's EVPN zone must be in the inventory group `pve` with root SSH. On a first install stage
55 says SKIPPED until a tenant network exists: create one in the customer console, then run `make install` again.
Stage 60 registers `<name> Sandboxes` as `DISABLED`; open it, run **Check release gates**, then set it `PUBLIC`.
Public sandbox ports are optional; leave them out of a first install. They take `sandbox.domain`,
`sandbox.proxy.address` (inside `sandbox.network.metallb`) and a certificate: PowerDNS for the default `acme` issuer,
or `sandbox.tls.issuer: secret` with your own wildcard certificate. Without one of those, stage 56 fails:
[install.md § Sandbox ports](install.md#sandbox-ports-stage-56).

### E. Install on Kubernetes with Helm

The chart installs the API and both consoles. It does not create OS templates, Proxmox tokens or providers: register
those in the operator console as in [A2](#a2-connect-an-existing-proxmox-cluster). Chart reference:
[deploy/charts/proxcloud/README.md](../deploy/charts/proxcloud/README.md).

**Before you start:** Kubernetes 1.25+, Helm 3.8+, `kubectl` and a default StorageClass.

**Images.** The chart pulls `ghcr.io/mutex-solutions/proxcloud-{api,web,admin}` at its `appVersion`. Until that
release is published, build the images from this checkout and load them into the cluster (kind shown):

```sh
docker build -t proxcloud-api:local -f deploy/Dockerfile .
docker build -t proxcloud-web:local --build-arg APP=client web
docker build -t proxcloud-admin:local --build-arg APP=admin web
kind load docker-image proxcloud-api:local proxcloud-web:local proxcloud-admin:local
```

and add `--set api.image.repository=proxcloud-api,api.image.tag=local,ui.image.repository=proxcloud-web,ui.image.tag=local,admin.image.repository=proxcloud-admin,admin.image.tag=local`
to the `helm install` below. Without it the pods stay in `ImagePullBackOff`.

**Quick start, no operators.** One command brings up the API, both consoles, PostgreSQL-backed jobs and Keycloak with
its realms:

```sh
helm repo add codecentric https://codecentric.github.io/helm-charts
helm repo add cnpg https://cloudnative-pg.github.io/charts
helm dependency build deploy/charts/proxcloud
helm install proxcloud deploy/charts/proxcloud -n proxcloud --create-namespace --wait --timeout 15m \
  -f deploy/charts/proxcloud/values-quickstart.yaml \
  --set api.encryptionKey=$(openssl rand -hex 32) \
  --set postgresql.auth.password=$(openssl rand -hex 16) \
  --set keycloakConfigCli.adminPassword=$(openssl rand -hex 12)
```

The secrets have no defaults; `helm get values proxcloud -n proxcloud` reads them back. The release notes print the
port-forwards; run them and keep them running:

```sh
kubectl -n proxcloud port-forward svc/proxcloud-api 8180:8080 &
kubectl -n proxcloud port-forward svc/proxcloud-ui 8182:80 &
kubectl -n proxcloud port-forward svc/proxcloud-admin 8183:80 &
kubectl -n proxcloud port-forward svc/proxcloud-keycloak-http 8190:80 &
```

| What | URL | Login |
|---|---|---|
| Customer console | `http://localhost:8182` | Register an account on the sign-in page (realm `proxcloud`) |
| Operator console | `http://localhost:8183` | `admin` and `kubectl -n proxcloud get secret proxcloud-keycloak-admin -o jsonpath='{.data.admin-password}' \| base64 -d` |
| Keycloak | `http://localhost:8190` | The same `admin` |

The URLs are written into the tokens, so keep the ports. A seed Job has already written the platform and billing
configuration. Then register a Proxmox provider ([A2](#a2-connect-an-existing-proxmox-cluster)) and seed the customer
basics through the port-forwards:

```sh
PROXCLOUD_API=http://localhost:8180/api/v1 \
PROXCLOUD_TOKEN_URL=http://localhost:8190/realms/master/protocol/openid-connect/token \
PROXCLOUD_ADMIN_PASSWORD="$(kubectl -n proxcloud get secret proxcloud-keycloak-admin -o jsonpath='{.data.admin-password}' | base64 -d)" \
deploy/seed/seed-operator.py --provider "<provider name>"
```

The quick start is not for production: one replica of everything, plain HTTP, and Keycloak's own administrator is
the operator. To expose it with an Ingress, see the chart README.

**Production-oriented configuration, with CloudNativePG.** This is a deployment recipe,
not a stable-release or clean public-artifact install acceptance claim. It needs the
CloudNativePG operator, an ingress controller with TLS, and an OIDC issuer with the
realms and clients in [auth.md](auth.md). No RabbitMQ broker/operator is needed. Before upgrading an existing release, follow the [drain/reconcile and
retain runbook](postgres-jobs-migration.md); do not let Helm/Argo prune the old broker.

1. Fetch the subcharts (the two `helm repo add` and `helm dependency build` lines above).

2. `my-values.yaml`:

   ```yaml
   api:
     selfUrls: {base: https://app.example.org, api: https://api.example.org,
                ui: https://app.example.org, admin: https://admin.example.org}
     jobs: {schedulerEnabled: true}    # billing and sync crons; off by default
     ingress: {enabled: true, className: nginx, host: api.example.org}
   ui:
     ingress: {enabled: true, className: nginx, host: app.example.org}
   admin:
     ingress: {enabled: true, className: nginx, host: admin.example.org}
   auth:
     main: {issuer: https://id.example.org/realms/proxcloud}
     admin: {issuer: https://id.example.org/realms/proxcloud-admin}
     adminApi: {issuer: https://id.example.org/realms/proxcloud-admin}
   ```

   To bundle Keycloak instead of bringing an issuer, copy the `keycloakx` and `keycloakConfigCli` blocks of
   `values-quickstart.yaml`: Keycloak needs a database (`cnpg.keycloak` adds one to the CloudNativePG cluster), its
   bootstrap admin password, and `keycloakConfigCli.adminPassword`, which the chart writes to the Secret
   `<release>-keycloak-admin` that both read.

3. Install. `api.encryptionKey` and `postgresql.auth.password` are required; the chart refuses to render without them.

   ```sh
   helm install proxcloud deploy/charts/proxcloud -n proxcloud --create-namespace -f my-values.yaml \
     --set api.encryptionKey="$(openssl rand -hex 32)" \
     --set postgresql.auth.password="$(openssl rand -hex 16)"
   ```

   Keep the encryption key: stored provider secrets cannot be read without it.

4. Register a Proxmox provider, then seed the customer basics from any machine that reaches the API and the issuer:

   ```sh
   PROXCLOUD_API=https://api.example.org/api/v1 \
   PROXCLOUD_TOKEN_URL=https://id.example.org/realms/proxcloud-admin/protocol/openid-connect/token \
   PROXCLOUD_ADMIN_USER=<operator> PROXCLOUD_ADMIN_PASSWORD=<password> \
   deploy/seed/seed-operator.py --provider "<provider name>"
   ```

   The operator client (`proxcloud-admin`) must allow the password grant; the clients `keycloakConfigCli` creates do.

## 3. Storage tier: Standard or Full

| | Standard | Full |
|---|---|---|
| VM disks | Local ZFS (profile `std`, detected), shared LVM on a TrueNAS iSCSI LUN (added by hand) | Ceph RBD (profile `ceph`, detected) |
| Templates, images, backups | NFS stores from the NAS (`nas-templates`, `nas-capacity`, `nas-backup`) | Templates on RBD (one copy serves every node); backups on CephFS or PBS (`backupStorage`) |
| Tenant file shares | TrueNAS NFS and SMB (provider `truenas`) | Not built yet for CephFS |
| S3 for tenants | RustFS, deployed and registered by the installer | Ceph RGW, registered by the installer as `ceph-s3` |
| Installer keys | `nas.host`, `nas.exports`, `object_store.mode: vm` or `truenas-app` | `object_store.mode: ceph-rgw` + `endpoint`, `access_key`, `secret_key`, `region` |

Profiles are detected by preflight from the PVE stores; `storage_profiles` and `storage_classes` override them.

**Standard, with a TrueNAS:**

```yaml
proxcloud:
  nas:
    host: 192.0.2.20
    exports: {templates: /mnt/tank/templates, capacity: /mnt/tank/capacity, backup: /mnt/tank/backup}
  object_store:
    mode: vm                            # or truenas-app: RustFS as a TrueNAS app,
                                        # with nas.pool and vault truenas_api_key
```

Stage 05 adds the three NFS stores; stage 60 sets the provider's `backupStorage` to `nas-backup` and `importStorage`
to `nas-templates`. Tenant NFS/SMB shares: operator console → Cloud providers → Add provider → TrueNAS
([cloud-integration.md § TrueNAS file shares](cloud-integration.md#truenas-file-shares)). Shared block on an iSCSI LUN:
[cloud-integration.md § Shared block](cloud-integration.md#shared-block-on-a-truenas-lun).

**Full, with Ceph:** add the RBD (and CephFS) stores in PVE first; preflight then proposes the `ceph` profile and makes
it the default. For S3:

```yaml
proxcloud:
  object_store:
    mode: ceph-rgw
    endpoint: https://s3.example.org
    access_key: <RGW admin user, caps users=*;buckets=*;usage=*>
    secret_key: <its secret>
    region: us-east-1                   # the zonegroup's api_name
```

`mode: split` refuses `ceph-rgw` (database backups go to the installer's own RustFS bucket). A split Full-tier site
keeps `object_store.mode: vm` and adds Ceph RGW for tenants in the console: Add provider → Ceph S3.

## 4. Add-ons

| Add-on | What to do | Read |
|---|---|---|
| GPUs | Create a PCI resource mapping in PVE (Datacenter → Resource Mappings), list it in the provider's `gpus` block (`{"<mapping-id>": {"alias": "nvidia-a6000"}}`), add a GPU flavor (System → Flavors (Proxmox)) and a price rule on `gpu_model`. The installer already grants `/mapping/pci` | Operator manual `/docs` → Connect a cloud → GPU |
| DNS zones | A PowerDNS server with `api=yes`, then Add provider → PowerDNS (API URL, server id, name servers, API key) | [cloud-integration.md § PowerDNS](cloud-integration.md#powerdns-zones) |
| Load balancers | Built into the Proxmox provider: an HAProxy VM per balancer. Needs `network.public` and a Debian-family image (`lbImage`, default `debian-13`) | [cloud-integration.md § Load balancers](cloud-integration.md#load-balancers-haproxy-appliance) |
| Server backups | With a NAS: `nas.exports.backup`, then `make install` sets `backupStorage`. Otherwise set the provider's `backupStorage` to a shared store with `backup` content (NFS, CephFS, PBS) through `PUT /api/v1/admin/service/{id}/update` | [cloud-integration.md](cloud-integration.md#the-proxmox-provider-document) |
| Per-organization SSO | On by default with the installer. Restrict Keycloak's egress before you let tenants use it. Helm: `auth.sso.enabled`, `auth.sso.clientSecret`, `keycloakConfigCli.enabled` | [install.md § Per-organization SSO](install.md#per-organization-sso-restrict-keycloaks-egress), [auth.md](auth.md) |
| Payments | System → Integrations → Stripe (or bank transfer). Activation rules: System → Billing configuration → Activation. Mail: an SMTP integration, selected as Mail gateway in Billing configuration | Operator manual `/docs` → Account activation, Invoicing |

## 5. After any install

The installer's stage 60 already did steps 1 and 2. The other paths do them by hand.

1. **Provider.** Operator console → System → Cloud providers: one Proxmox provider, **Test connection** green. Read
   any warning about missing privileges.
2. **Price plan, flavors, quota.** `deploy/seed/seed-operator.py` (stage 60 runs it once) creates the public price
   plan, the flavor catalog and the default quota. Review them under System → Price plans and System → Flavors
   (Proxmox); the seed's rates are placeholders.
3. **First tenant.** Sign up on the customer console (self-registration is on), create an organization and a project,
   fill the billing details. Activate the billing profile: Client area → Billing profiles → Activate, or turn on
   auto-activation. The project is then bootstrapped onto the provider (pool, default security group, quota).
4. **First server.** Create a network, then a server from a template. On split sites, create the network before
   re-running stage 55 (sandbox).

## 6. What each stage installs

`make install` runs them in this order (`installer/playbooks/site.yml`); each is idempotent and skips itself when its
switch is off.

| Stage | Installs | Runs when | Default |
|---|---|---|---|
| 00 preflight | Checks only: PVE version, firewall, FRR, reach, storage detection, certificate pin, fio | always | on; `preflight.firewall: warn` softens the firewall check |
| 05 storage | NFS stores `nas-templates`, `nas-capacity`, `nas-backup` | `nas.host` set | off |
| 10 identity | PVE roles, users, tokens, ACLs, pools | always | on |
| 25 mgmt VMs | Management VMs on PVE | `mgmt.placement: pve-vm` | off (`ssh`) |
| 20 templates | OS templates (`templates.os`, default Debian 13, Ubuntu 24.04) | always; needs `templates.build_ip` | on |
| 30 mgmt cluster | k3s | always | on |
| 35 local images | API, web and admin images and the chart, built from this checkout into a registry on k3s | `images.build: true` | on |
| 40 platform | Migration gate, ArgoCD, cert-manager, CloudNativePG, broker-free ProxCloud with Keycloak; scheduler/debug off unless opted in | always | on |
| 45 object store | RustFS VM, or RustFS as a TrueNAS app, plus the internal buckets | `object_store.mode` `vm` / `truenas-app` | `vm` |
| 50 services cluster | Kamaji, Cluster API, MetalLB, proxmox-csi, database operators | `mode: split` | off |
| 55 sandbox cluster | The sandbox vnet, SNAT, cluster and `deploy/sandbox-cluster` | `mode: split` + `sandbox.enabled` | off |
| 55 sandbox registry | Zot on the services cluster, its bucket, image copies and prepull | the above + `sandbox.registry.address` | off |
| 56 sandbox edge | sandbox-proxy, wildcard certificate, DNS record, HAProxy entry VM | the above + `sandbox.domain` | off |
| 60 register | Logins, Admin-API key, Proxmox provider, object store provider, seed, managed-service providers; prints the URLs | always | on |
| 70 / 71 | Add or remove a PVE node: `make add-node NODE=pve4`, `make remove-node NODE=pve3` | on demand | |

Versions are pinned in `installer/playbooks/group_vars/all/versions.yml`; every option and default is in
`installer/playbooks/group_vars/all/defaults.yml`. Override both under `proxcloud:` in the site's group_vars.

## 7. Troubleshooting a first install

| Symptom | Cause | Fix |
|---|---|---|
| Preflight: `the datacenter firewall is off` or `node ... does not run the nftables firewall`; or a failed `Check FRR on the nodes` | Tenant security groups need both firewalls; EVPN needs `frr-pythontools` | Turn both firewalls on (A2 step 2). Install `frr-pythontools` on every node, or use `network.kind: simple` on one node |
| Preflight fails at `Refuse a Proxmox host` | The `mgmt` host is a PVE node; k3s and pve-firewall fight over FORWARD | Use another Linux host, or `mgmt.placement: pve-vm` (path B) |
| Network create answers 403, or Test connection lists missing `SDN.*` privileges, after an ACL was added by hand | A role on a child path (for example `/sdn/zones`) replaces what the parent grants that user or token | Grant `ProxCloudPlatform` on that same path too, then read the effective rights: `pveum user token permissions proxcloud@pve platform --path /sdn/zones` |
| Stage 05 fails adding `nas-*`, or a node cannot mount it | The TrueNAS NFS share does not allow the PVE nodes, or squashes root | Allow the nodes' management subnet on each export in `nas.exports` and map root to root (PVE writes as root). Tenant shares are separate: [NAT and exports](cloud-integration.md#truenas-file-shares) |
| Stage 55 stops on `sandbox.network.snat is required` or on the exit node | The sandbox needs its own egress address, placed on exactly one exit node over root SSH | Set `sandbox.network.snat` (or `node`), give the zone one exit node, put it in the inventory group `pve` |
| Sandbox stages cannot pull `proxcloud-sandbox-proxy` or `proxcloud-sandbox-runtime` on an `images.build` site | Stage 35 builds only API/web/admin; sandbox stages default to the BOM release tag (`v0.5.0`), which exists only after its tag's image workflow succeeds. Public development images do not create release tags | Pin `sandbox.proxy.image` and `sandbox.runtime_image` to exact compatible images/digests your nodes can pull; verify the BOM tag is pullable before relying on it |
| Template build fails downloading the vendor image | No store with `import` content | Add `import` to a store's content, or set `templates.import_storage` |

Still stuck: review the failed stage and migration gates before a verbose re-run, `make install INVENTORY=inventory/mysite ARGS=-v` (this can reconcile resources, not just read them),
and read [install.md](install.md).
