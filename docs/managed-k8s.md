# Managed Kubernetes (kamaji provider) — operator runbook

How to register, operate and debug the `kamaji` managed-Kubernetes provider. A
kamaji provider is a **management cluster** reached over a stored kubeconfig:
each customer cluster is one ArgoCD `Application` that ProxCloud writes to that
cluster's API, and Kamaji runs the cluster's control plane as pods there
(`TenantControlPlane`).

> ## Status on Proxmox
>
> **Evidence boundary:** the operations below describe historical development-lab
> acceptance of the managed-provider flow, not a blanket production certification
> or a clean install from published release artifacts. The new static ISO template
> builder is a separate path: configuration/tests pass, but live ISO boot, installed
> reboot and Goss acceptance have not yet run.
>
> Creating a cluster was verified on that lab: a tenant control plane on Kamaji, worker VMs through CAPMOX, Cilium and MetalLB, with
> workers in the project's pool on the project's network, under tokens minted per cluster and revoked after the delete.
> Verified on PVE 9.2 with CAPI v1.14.4, CAPMOX v0.9.2 and Kamaji CP provider v0.22.0: create, scale-out, RWX volumes on a
> NAS dataset, a tenant LoadBalancer, project suspend and resume, a refused create, delete with everything it held.
> **Block volumes (proxmox-csi) come through the CSI gateway**, which keeps every PVE credential out of the tenant
> cluster: a new cluster gets them when the portal runs the gateway (on in the installer's split mode), the compute
> provider holds its token and the Kubernetes provider pins chart 0.1.7 or later. Without the gateway a cluster gets
> no block volumes: see [section 9](#9-how-a-cluster-lands-on-proxmox). Not available: autoscaling node groups, GPU node groups. Not run: the
> installer's split mode end to end, and a NAS the workers reach without SNAT (the lab has none).

Code: `internal/cloud/kamaji` (config, values builder, Application builder,
service, sync provider), `internal/cloud/kamajik8s` (minimal management-cluster
API client), `internal/platform/project/cloud_kamaji.go` (the
`KUBERNETES_CLUSTER` surface), `internal/platform/externalservice/kamaji.go`
(provider accessors).

## Gateway API for new clusters

Cluster chart `0.1.10` offers **NGINX Gateway Fabric** (`nginxGatewayFabric`) instead
of a new ingress-nginx installation. The addon is opt-in and includes pinned tenant
Gateway API Standard prerequisites, but creates no application Gateway, route, TLS
issuer or DNS entry. Existing legacy ingress releases and their traffic are preserved,
not automatically migrated by an addon edit or platform bump. See the
[Gateway API contract, CRD lifecycle and migration runbook](managed-k8s-gateway-api.md).
Source implementation and offline rendering are not a live Gateway API canary.

## 1. Management-cluster prerequisites

One-time, per management cluster (apply order:
[`deploy/mgmt-cluster/README.md`](../deploy/mgmt-cluster/README.md)):

1. The Kamaji stack is running (Kamaji operator and its DataStore, plus the
   Cluster API machinery the cluster chart uses).
2. **ArgoCD is installed.** ArgoCD is the delivery plane: ProxCloud only writes
   `Application` objects through the Kubernetes API and never talks to the ArgoCD
   API.
3. `deploy/mgmt-cluster/` is applied: `rbac.yaml` (ProxCloud ServiceAccount,
   least-privilege RBAC, token), `appproject.yaml` (AppProject `proxcloud-k8s`,
   the source/destination guardrail), `argocd-health.yaml` (custom health checks
   for `TenantControlPlane` / `Cluster` / `MachineDeployment`).
4. **Chart pinning.** Publish/copy the cluster chart to the registry your ArgoCD
   can reach and verify the exact version exists before registering it. A chart
   directory/version in this checkout is **not** proof of a public OCI artifact;
   no public ProxCloud chart release has been published yet. Installer stage 50
   packages the checkout's charts into its services-cluster registry, a separate
   path from the GitHub tag-based chart workflow. Never `latest`: the provider and
   every existing cluster pin an exact version, and the registry must keep it.
   The values contract lives in `internal/cloud/kamaji/values.go` and is pinned by
   `TestBuildValues`. The chart is `deploy/charts/proxmox-kamaji-cluster`, and
   `config.argocd.chartName` has no default: the provider config must name it.

### Node-template build network

Building a Kubernetes node image does **not** require DHCP as a Kubernetes
prerequisite. The manual `k8s-node-templates` Actions workflow defaults to
`network_mode=static`: existing `BUILD_IP` (host CIDR), `BUILD_GATEWAY`,
`BUILD_NAMESERVER` and optional build VLAN Repository Secrets generate a private
local NoCloud `cidata` ISO. The live installer and installed build OS receive the
same fixed-MAC Netplan configuration; Packer has an explicit SSH address, with no
guest-to-runner HTTP seed route. Direct script use still defaults to legacy DHCP,
so set `PVE_BUILD_NETWORK_MODE=static` explicitly.

The runner needs xorriso/OpenSSL and access to template disks plus an ISO-capable
store (`PVE_ISO_STORAGE`, optionally different from `PVE_TEMPLATE_STORAGE`). Keep
the reserved build address outside all allocators. Static guest provisioning,
reboot and Goss precede final guest sanitization, exact seed detachment/deletion
and build-NIC removal; private logs/seed files must never become public artifacts.
A killed runner is not cleanup proof. Do not adopt an old same-name template as
clean just because its NIC was removed, or start a competing DHCP server as a
workaround. See the [template pipeline](../images/README.md#static-mode-workflow-default-no-dhcp)
and [quickstart command](quickstart.md#d-managed-kubernetes-databases-and-sandboxes-split).
The installer consumes the output node→VMID map; stage 50 does not call this build
or import Actions Secrets. Runtime acceptance of the new static ISO path remains
pending for the chosen Kubernetes version; successful Debian/Ubuntu OS cloud-image
canaries do not prove it.

## 2. Provider setup (admin → Cloud providers → Add provider → Kubernetes)

Seed-file twin of the form: `deploy/seed/external-service-dev.json`
(`svc-kamaji-dev`).

| Form field | Config key | Notes |
|---|---|---|
| Management kubeconfig | `secret.kubeconfig` | The ProxCloud ServiceAccount kubeconfig assembled per the recipe in `deploy/mgmt-cluster/rbac.yaml`. Plain token or client certificate only, no exec plugins. Encrypted at rest. |
| Regions | `config.regions` | The region(s) stamped on cached clusters. |
| Services | `config.services` | `{"kubernetes": {"<region>": true}}` — drives the client-portal nav gating and the sync. |
| ArgoCD namespace / project | `config.argocd.namespace` / `.project` | Must match `deploy/mgmt-cluster` (`argocd` / `proxcloud-k8s`). |
| Chart repo / name / version | `config.argocd.chartRepo` / `.chartName` / `.chartVersion` | All three required. OCI repo **without** the `oci://` prefix. The version is the pin for NEW clusters; existing clusters keep their own pin until moved (§3). |
| DataStore | `config.cluster.dataStoreName` | Kamaji DataStore name (default `default`). |
| DNS zone | `config.cluster.dnsZone` | Optional. API FQDN = `<clusterId>.<zone>`. Cluster ids (`pc-c-<8hex>`) never change, and a cluster made before this format keeps the id it has; nothing reads the prefix. |
| Version → image matrix | `config.cluster.versions` | Curated map `k8s version → node image id`: the only versions offered to customers. Textarea grammar: `1.35.4=<image-id>` per line. |
| Image variants | `config.cluster.imageVariants` | Alternative node-image builds of the same version, e.g. a GPU build. Grammar: `1.35.4@nvidia=<image-id>`. |
| RWX dataset size | `config.cluster.rwxGiB` | Optional, API or installer (`services.kamaji.rwx_gib`). See [section 9](#9-how-a-cluster-lands-on-proxmox). |
| Block volumes for tenants | — | Through the CSI gateway only, with no credential in the tenant ([section 9](#9-how-a-cluster-lands-on-proxmox)). `csiSharedStores` and `pveCsiUrl` were removed: the API answers 400 to a `cluster` block that still has one, and the form drops them on save. |
| Flavors allowlist | `config.cluster.flavors` | Flavor ids offered for node groups. Empty = no restriction. |
| Namespace placement | `config.namespacePlacement` | Optional, API only: `{nodeSelector, tolerations}`. A `pc-*` namespace gets the `scheduler.alpha.kubernetes.io/node-selector` and `…/defaultTolerations` annotations and the label `proxcloud.io/platform-role: kamaji`, so the API server puts new tenant control-plane pods there on that node group. They are stamped when the namespace is next ensured: a project's first attach to the provider, or a new cluster create. Nothing in the sync ensures a namespace again, so a project attached before the key was set keeps its namespace as it is until then, and pods already running are not moved. Needs the `PodNodeSelector` and `PodTolerationRestriction` admission plugins on the management cluster's API server; without them the annotations do nothing. Absent: nothing is stamped. A selector that is not label keys and values, or a toleration the API server would refuse, is refused when the provider is built: the API server checks the annotation on every pod create, so it would refuse every pod there. |

**Changing a setting later.** The installer's `services.kamaji.*` keys take effect only when stage 60 first registers the provider: it skips a provider
whose name already exists, so a re-run neither applies a changed key nor overwrites one set by hand. On an existing site use the
form above, or for the keys it has no field for (`rwxGiB`) the API. An update replaces each top-level key of
`config` whole, so send the complete `cluster` block, or you lose its versions and URLs:

```sh
# AUTH='Authorization: Bearer <operator token>', API=<api url>/api/v1, ID=<provider id>
curl -s -H "$AUTH" "$API/admin/service/$ID" | jq '{config: {cluster: (.data.config.cluster | del(.csiSharedStores, .pveCsiUrl)) + {rwxGiB: 100}}}' \
  | curl -s -X PUT -H "$AUTH" -H 'Content-Type: application/json' -d @- "$API/admin/service/$ID"
```

It applies to clusters created afterwards. Existing clusters keep what they were created with: their dataset, their MTU, their
StorageClasses and any CSI token (see "Taking the token back" in [section 9](#9-how-a-cluster-lands-on-proxmox)).
A numeric key may be a number or a string of digits, since the installer sends strings.

## 3. How a cluster is represented

```mermaid
sequenceDiagram
    participant C as Customer
    participant S as ProxCloud
    participant A as ArgoCD (mgmt cluster)
    participant KJ as Kamaji / Cluster API
    C->>S: Read cluster / list clusters
    S->>A: Read Application (health, sync)
    S->>KJ: Read TenantControlPlane and MachineDeployments
    S-->>C: Status
    C->>S: Download kubeconfig
    S->>KJ: Fetch the admin-kubeconfig Secret on demand (never stored)
    C->>S: Delete cluster
    S->>A: Delete Application (cascades through the rendered objects)
```

- Each cluster is one ArgoCD `Application` named `<clusterId>` in the ArgoCD
  namespace: source = the pinned chart, full generated values inline,
  destination = namespace `pc-<projectId>`, project `proxcloud-k8s`, finalizer
  `resources-finalizer.argocd.argoproj.io`.
- **Ownership marker:** ProxCloud only lists, patches and deletes objects labeled
  `app.kubernetes.io/managed-by: proxcloud`. Anything else on the management
  cluster is invisible to it.
- **Sync** (`syncjob.syncKamajiService`, every 15 minutes) reconciles the
  project's `KUBERNETES_CLUSTER` rows from Applications labeled with the project
  id, enriched from the TenantControlPlane and MachineDeployments.
- **Post-create changes** (k8s upgrade, node groups, OIDC, add-ons, kubeconfig
  rotation, platform update) all mutate the stored desired spec and re-apply the
  whole Application. Every cluster pins the chart version it was created with;
  the provider's `chartVersion` only affects new clusters. An existing cluster
  moves through the customer's *Apply platform update* or the admin's per-cluster
  **Bump** / **Bump all** on the provider page. The admin Bump was run on Proxmox
  (0.1.5 to 0.1.6 on the lab: ArgoCD synced the new pin, the workers stayed); the
  customer's *Apply platform update* has not been.

### Deletion and orphan finalization

Delete removes the Application; the resources finalizer makes ArgoCD cascade the
delete through everything the chart rendered. The delete answers `202` before
that cascade has run. What must outlive the cascade is released after it: the
cluster's Proxmox token `capmox@pve!<clusterId>` (and a `csi@pve!<clusterId>`
an older cluster may hold), its addresses, its RWX dataset, its per-cluster
Secrets and, when the project's last cluster is gone, the `pc-*` namespace. The
delete itself cannot do that: CAPMOX deletes the workers with that token.

Two things do the release, with the same checks and the same steps:

1. **The release job** (`kamaji.release/v1`, `project.KamajiReleaseRunner`). The
   delete, and a project teardown, queue one durable job per cluster
   ([durable-jobs.md](durable-jobs.md)). Its runner is part of the API process
   and runs with or without the job scheduler. It looks at the cluster 15
   seconds after the delete and then at growing intervals, a quarter of the
   job's age, at most every 5 minutes, and releases the cluster at the first look
   that finds the cascade over. After 24 hours without that it gives up: the job
   is `dead` with `cascade_not_finished` in `GET /api/v1/admin/durable-jobs`, and
   the sweep is what is left. A dead release job is not replayed (`409`).
2. **The sweep** (`syncjob.sweepKamajiOrphans`), the backstop: once per provider
   sync, for every marker Secret of the provider on the management cluster. It
   also reaps leftovers of projects whose ProxCloud record is already gone, and
   of a create that died halfway. It never touches a marker younger than 30
   minutes, counted from the cluster's **creation**: a sweep that finds a marker
   with no Application cannot tell a finished delete from a create that has not
   applied its Application yet. The job has no such wait, because a delete is
   accepted only for a cluster whose Application was applied.

**When the cascade counts as over.** All of: no Application, no
TenantControlPlane, no MachineDeployment and no Machine of the cluster on the
management cluster, and no VM whose name starts with `<clusterId>-` in the
project's pool on Proxmox (CAPMOX names a VM after its Machine). A VM of the
pool on a node that is down has no name in Proxmox's list, so it holds the
release as well until the node is back or the VM is gone. While any of these
is left, or the management cluster or Proxmox does not answer, nothing is
revoked and the job looks again later (`cascade_running` or `release_failed` as
the job's last result; the reason is in the API log, line `kamaji: release of a
deleted cluster, will retry`). On a management cluster whose ClusterRole is
older than the Machines grant ([Provisioning status](#provisioning-status-reason-and-needs-attention), "RBAC") the
Machines cannot be listed; the other checks decide there.

**How long a deleted cluster's Proxmox token lives.** With the release job: until
the first look after the cascade, so seconds to a few minutes after the last
worker is gone, on every install. A lab delete on 2026-11-17 had its worker VM
gone 45 seconds after the delete; in a job's first minute the looks are 15
seconds apart. **This timing is not verified on a lab**:
the job is covered by tests with a fake management cluster and a fake Proxmox
on PostgreSQL. Should the job not have run (an API from before v0.8.2, a job
that could not be queued, a job that gave up), the sweep's timing applies: with
the scheduler the first provider sync after the cascade and after the marker's
30 minutes; without the scheduler (the Compose install's default) not until an
operator starts a provider sync
(`POST /api/v1/admin/job/external-service/sync`). That older timing was seen on
a lab (2026-11-17, v0.8.0, no scheduler): a token still listed 3 minutes after
the delete went one second after an operator's sync, and the token of a cluster
deleted 22 minutes after its creation survived two syncs.

Until the release the token is listed in Proxmox with its ACL rows
(`ProxCloudCAPI` on the project pool, the boot store, the vnet and `/nodes`).
Its secret lives only in a Secret of the cluster's namespace on the management
cluster, which the cascade deletes, so what is left is a credential nobody
holds, not one in use. Proxmox stops honouring the
token's ACL rows at the revoke, but leaves their lines in `/etc/pve/user.cfg`
until that file is next written; until then every `pveum`, `qm` and `pvesh`
call on a node prints `user config - ignore invalid acl token
'capmox@pve!<clusterId>'`, once per row. It is harmless, and `pveum acl list`
shows the rows gone at once.

Project teardown also removes the `pc-*` namespace of a project that never had a
cluster, once nothing is pending and no Application of the project is left.

```sh
# namespaces ProxCloud owns, with what's left inside them
kubectl get ns -l app.kubernetes.io/managed-by=proxcloud
kubectl get applications.argoproj.io -n argocd -l app.kubernetes.io/managed-by=proxcloud
kubectl get tenantcontrolplanes,machinedeployments -n pc-<projectId>
```

### Provisioning status: reason and "needs attention"

`cluster.status` stays what ArgoCD reports (PENDING, PROGRESSING, READY, DEGRADED). Next to it the sync says why a
cluster is not there yet. While a node group is unsettled (a MachineDeployment whose `replicas`, `readyReplicas` or
`upToDateReplicas` differ from what it asks for, or a Machine that is not Running with a Ready node), its entry in
`cluster.node_groups` carries `progress = {code, since, machines, attention}`:

- `code`: the reason of the group's worst machine (one that needs attention now, then the least advanced, then the
  oldest). The table below has the codes.
- `since`: when the group last made progress (RFC 3339).
- `machines`: how many machines share the code.
- `attention`: the group made no progress for longer than the bound of its least advanced machine, or the code is one
  that is not retried.

`cluster.attention = {code, since}` is the same for the whole cluster: `control_plane_stalled` or
`provider_access_refused`. It carries control-plane and provider codes only: **a worker that stalls is reported on
its node group's `progress.attention`, never on `cluster.attention`**, so a client that wants "this cluster needs
attention" reads both (seen on a lab: a worker that could not join set `node_groups[0].progress.attention` after
its 15 minutes, and the cluster had no `attention` key). Both fields are absent on a settled cluster. Attention is
not a terminal state: it is computed again at every sync and goes away when the cluster recovers. It is evaluated
only at a provider sync, so with the scheduler's 15-minute sync it can show up to one interval after the bound has
passed, and on an install without the scheduler only when an operator starts a sync. The consoles show an amber **Needs attention**
chip on the list and the cluster page, and read a cluster that needs attention once a minute instead of every 15 s.

The source is what Cluster API and CAPMOX report on the management cluster: `Machine`
(`cluster.x-k8s.io/v1beta2`), `ProxmoxMachine` and `ProxmoxCluster`
(`infrastructure.cluster.x-k8s.io/v1alpha2`). They are read once per project sync, and only when a cluster has
something to explain. Only the code and the time are stored: an upstream message names PVE nodes, VMIDs and storage
paths and never reaches a tenant. The first match in this order wins:

| Code | When | The tenant reads | Attention |
|---|---|---|---|
| `provider_access_refused` | A ProxmoxCluster or ProxmoxMachine condition that is not True carries Proxmox's refusal of the token ("authentication failure", "not authorized", "401 unauthorized"), or the ProxmoxCluster has no `ProxmoxAvailable` condition 10 min after it was made (CAPMOX reports only after it opened the Proxmox API) | The platform cannot reach the virtualization provider. Contact the operator. | at once |
| `no_capacity` | The Machine's failure message or the ProxmoxMachine's condition says CAPMOX could not reserve memory on the node (`cannot reserve ... of memory on node`, reason `VMProvisionFailed` at CAPMOX v0.9.2; a condition counts only with that reason, so a clone that is retried is `vm_create_retrying` whatever its message says) | The host has no free capacity for this machine, and it is not retried. Reduce the count or pick a smaller flavor; to retry, reduce the count by one and raise it again. | at once |
| `vm_create_failed` | Reason `VMProvisionFailed` or `TaskFailed`, phase `Failed`, or a failure message on the Machine: CAPMOX skips the machine from then on | Creating the virtual machine failed and is not retried. Reduce the count by one and raise it again, or delete and recreate the cluster. | at once |
| `vm_create_retrying` | Reason `CloningFailed` or `PoweringOnFailed` | Creating the virtual machine failed; retrying automatically. | by bound |
| `node_not_ready` | Phase Running, `NodeReady` not True | The node joined but is not Ready. Check it with kubectl. | 20 min |
| `waiting_control_plane` | Phase Pending and the control plane is not Ready | Waiting for the control plane. | never |
| `creating_vm` | Reason `Cloning`, `WaitingForDiskReconciliation`, `WaitingForStaticIPAllocation`, `WaitingForBootstrapDataReconciliation`, or no ProxmoxMachine condition yet | Creating the virtual machine. | by bound |
| `starting_vm` | Reason `WaitingForVMPowerUp`, `WaitingForCloudInit`, `WaitingForBootstrapReady`, `WaitingForClusterAPIMachineAddresses` | Starting the virtual machine. | by bound |
| `not_joined` | Phase Provisioned, or `NodeHealthy` with reason `NodeDoesNotExist` | The machine runs but has not joined the cluster. Check that the cluster's network has a router or gateway and reaches the cluster endpoint, then reduce the count by one and raise it again. | 15 min |
| `provisioning` | Anything else, and a machine that Cluster API has not made yet | Preparing the machine. | by bound |
| `control_plane_stalled` | No endpoint, or the control plane has no status or is `Provisioning`, 15 min after the create | The control plane is taking longer than expected. Wait, delete and recreate, or contact the operator. | n/a |

The bounds, counted from the group's last progress (the newest of its machines' creation, `status.lastUpdated` and,
for a running machine, the last change of `NodeReady`):

| State of the least advanced machine | Bound |
|---|---|
| Not made yet (counted from the newest of the MachineDeployment's creation, ArgoCD's last finished sync and the last change of the cluster's values, which the portal stamps on the Application as `proxcloud.io/values-changed-at`: right after a scale-up the last sync is still the one before it) | 15 min |
| Pending | 15 min; never while the control plane is not Ready |
| Provisioning | 30 min, plus 5 min for each further machine of the cluster in Provisioning, at most 90 min. CAPMOX clones all machines at once, so on slow disks each one takes longer ([section 9](#9-how-a-cluster-lands-on-proxmox)) |
| Provisioned, no node | 15 min |
| Running, node not Ready | 20 min (the first node waits for the CNI) |

A machine that is being deleted (a scale-down, the old half of a rollout) counts neither as progress nor as a stall.
Not covered: a control plane stuck in a later upgrade and a machine stuck in Deleting have no clock; ArgoCD sync
errors and MachineHealthCheck are not read; the admin cluster list has no attention badge.

**Operator detail.** Admin console, the Kubernetes provider's page, **Managed clusters**, **Machines** on a cluster's
row (`GET /api/v1/admin/service/{id}/k8s-clusters/{clusterId}/machines`, permission `admin:service:read`): every
worker Machine read live, with its phase, VMID and PVE node, addresses, the same code and attention, and the
conditions with their upstream reason and message. Credentials are cut out of the messages (among them
`PVEAPIToken=user@realm!id=<secret>`) and each is clipped to 400 characters.

**RBAC.** The sync needs `get` and `list` on `machines.cluster.x-k8s.io` and on `proxmoxmachines` and
`proxmoxclusters.infrastructure.cluster.x-k8s.io`. `deploy/mgmt-cluster/rbac.yaml` and chart `proxcloud-platform`
0.1.2 grant them (the platform BOM pins 0.1.2, so a new managed-services install has them). On an install with the older ClusterRole nothing breaks: the sync leaves `progress` and `attention`
out (a 403 or 404 on the read), and the Machines dialog answers "The ProxCloud service account cannot read Machines:
re-apply deploy/mgmt-cluster/rbac.yaml". That advice holds on an installer-built site; on a managed-services platform
installed from an earlier BOM the `platform` Application reverts a hand-applied ClusterRole (self-heal), and the access
comes with Remove and a new install ([managed-services.md](managed-services.md)). Any other error on the read fails that project's sync, so the cached row
keeps its last state instead of losing the flag.

## 4. Billing

- `kubernetes_cluster` — the flat control-plane fee: a price-plan rule on
  attribute `existence` (e.g. `0.10`/hour), charged once the API endpoint exists
  (`internal/cloud/billingresource/kubernetescluster.go`). Worker VMs bill through
  the ordinary instance rules; never add k8s rules for them.
- Example rules live in `deploy/seed/price-plan-seed.json`.

## 5. Troubleshooting

Start from the ArgoCD Application — it aggregates everything the chart rendered:

```sh
# health + sync state of every ProxCloud cluster
kubectl get applications.argoproj.io -n argocd -l app.kubernetes.io/managed-by=proxcloud
# one cluster's full resource tree with per-resource health (or use the ArgoCD UI)
argocd app get <cluster-id> --show-operation
kubectl get application <cluster-id> -n argocd -o jsonpath='{.status.health}{"\n"}{.status.sync}'
```

| Symptom | Look at |
|---|---|
| Create answers `409 Could not mint the cluster's Proxmox tokens: the operator must fix a permission of the portal on Proxmox first. Nothing was created, and trying again does not help until then.` | Proxmox refused the portal token an ACL row for the cluster's token (a 403). The tenant's message names no path on purpose. The path is in Proxmox's own words, `Permission check failed (<path>, <privileges>)`, in two places: the operator's **Audit** page, on the `CLOUD_RESOURCE_CREATE` event with outcome `FAILURE` (`internalReason`), and the API log line `backend request failed` with the same request id as the answer's `X-Request-Id`. The usual cause is an ACL row of the portal's own on that store or zone, which hides the mint role its parent passes down: run the managed-services script again, then the Check ([proxmox-permissions.md](proxmox-permissions.md#a-row-of-the-portals-own-on-a-store-or-a-zone)). Until v0.8.0 this was a `502 The compute backend is unavailable` with no audit event. |
| Create answers `502 The compute backend is unavailable. Try again later.` | Proxmox did not answer, or failed for another reason than a permission. The reason is in the same two places. |
| Machines stuck, CAPMOX logs `not authorized to access endpoint` | PVE refused the cluster's token. The PVE access log (`/var/log/pveproxy/access.log`) names the call. `GET /cluster/status` answering 403 is normal and ignored. A refused clone is checked against the template's NICs too. |
| `error waiting for agent` for a few minutes | Normal while the clone boots. |
| Tenant `LoadBalancer` Service `<pending>` for up to 15 minutes after create | The MetalLB pool arrives by ClusterResourceSet, which retries until the chart's CRDs and MetalLB's webhook answer, with CAPI's growing backoff between tries: on the lab it applied 8 minutes after the cluster was Available. The Application reads `Progressing` until then; the portal shows the cluster READY once the control plane, every node group and the sync are done and the ClusterResourceSet is the one part left, for up to 30 minutes after create. A cluster still `PROGRESSING` past that: `kubectl get clusterresourceset -n pc-<projectId>` and its `ResourcesApplied` condition. |
| Application `OutOfSync` / `SyncFailed` | `argocd app get <cluster-id>` sync error. "not permitted in project proxcloud-k8s" → the chart needs a cluster-scoped resource; add exactly that group/kind to `deploy/mgmt-cluster/appproject.yaml`. Chart pull errors → registry or pin problem (§1.4). |
| Application Healthy but cluster unusable | Health checks not loaded or wrong: verify the `argocd-health.yaml` keys exist in `argocd-cm` and the application controller was restarted. |
| Cluster shows **Needs attention** | The reason is on the cluster page per node group; the table under "Provisioning status" in §3 has every code. The operator's detail is **Machines** on the provider page. `not_joined` on the lab of 2026-11-17 was a cluster network without a router: the VM ran, and its kubelet never reached the API endpoint. |
| Cluster stuck `PROGRESSING`, no endpoint | `kubectl get tcp -n pc-<projectId>` → TenantControlPlane status. Pending CP pods → management-cluster capacity or DataStore. |
| Delete hangs | The Application keeps its finalizer while the cascade runs; inspect what is left in the tree (`argocd app get`). Remove the finalizer by hand only if you accept orphaning the rendered objects, then clean up per §3. |
| Orphaned namespaces/secrets | §3 orphan-finalization check. |
| Cluster missing from the portal | The `kubernetes` service toggle for the region is off, or the Application lacks the ProxCloud labels. |

## 9. How a cluster lands on Proxmox

What the create path resolves (`internal/platform/project/cloud_kamaji_create.go`):

| What | From | Note |
|---|---|---|
| Workers' pool | the project's pool | they sync and bill as servers of their flavor (`fl-`, `sp-` tags) |
| Version | the customer's pick, a key of `config.cluster.versions` | a provider with no version (how managed services registers it, before a node image is published) refuses every create: `400 cluster: this provider offers no Kubernetes version yet…`, before addresses are reserved or tokens minted. An `UPGRADE` to a version that is not a key of `config.cluster.versions` answers `400 version is not offered by this provider`, on a provider with no version too, and changes nothing |
| Network | the customer's pick, one of the project's networks | the zone must have no DHCP server (EVPN, VLAN, or a `simple` zone without one). ProxCloud creates a `simple` zone with DHCP unless its config says `"dhcp": false` ([managed-services.md](managed-services.md#2-prerequisites)), so on a Proxmox provider whose default zone is a zone with DHCP a create answers `400 Worker nodes need a network without DHCP (an EVPN or VLAN zone, or a simple zone without a DHCP server)`; the managed-services Check reads the zone in Proxmox and warns about it (`tenant-network`). With shared workers (`cluster.shared`) no tenant network is used and this row does not apply |
| Quota | the project's limits, like servers | every worker counts against `servers`, `vcpus`, `ramMb` and `diskGb`; a create or node group edit over them answers 409 and takes nothing |
| Addresses | 16 for workers, 8 for tenant LoadBalancers, reserved from the top of the subnet | hence at most 15 workers: a rolling replacement needs a spare |
| Credentials | `capmox@pve!<id>` (management side); no CSI token, block volumes use the CSI gateway's | minted at create, revoked after the delete cascade (with a `csi@pve!<id>` an older cluster may still hold) |
| Bootstrap ISO store | the Proxmox provider's `config.isoStorage`, default `local` | must be node-local, see "Bootstrap ISOs" below |

**Node images.** `config.cluster.versions` maps a version to its template per PVE node, as printed by
`images/k8s-node-template.sh`: `{"1.35.10": "pve1=9210,pve2=9211"}`. PVE clones to local storage only on the
template's node, so a node group has one MachineDeployment per entry. The zone label comes from that pin, so no
cloud controller manager is needed. The image is the provider's: a node group in a request names an `imageVariant`
at most, and an `imageId` in a request is ignored (before v0.8.0 it went into the values as it came, so a caller of the
API could pin a group to nodes and template VMIDs of its choice; the console never sent it).

**Sites without shared storage: where a node group's replicas go.** On a site where every node has its own pool
(six nodes, each with a local ZFS pool), the image must exist on every node that should run workers
(`config.cluster.versions`: `"1.35.10": "pve1=9210,pve2=9211,..."`; [template-updates.md](template-updates.md#publish-on-all-nodes) has
one action that publishes a version on all nodes, one node at a time), and a worker's disk is on the node that runs it.

**All new workers are cloned at the same time.** A create or a scale-up makes one MachineDeployment per node that
gets replicas, and Cluster API starts the clones of all new workers together; ProxCloud does not queue them. On slow
shared storage that load is felt by other VMs. On a lab with ZFS on slow disks, three clones started in the same
second took 6 to 7 minutes each against 3.5 minutes for one alone, the load average reached 31 on one node, and the
controllers of another Kubernetes cluster on the same hosts restarted (its etcd waited up to 5 seconds for a
write). Create large node groups in steps where the storage is slow.

How many replicas each node runs depends on the cluster's chart version:

- **Chart 0.1.12 and older:** the chart splits `machineCount` evenly over the image string's entries, the first
  entries taking the remainder, blind to what is free. A full node still gets its share and its machines hang
  (`no_capacity` under "Provisioning status").
- **Chart 0.1.13 and later:** ProxCloud decides and stores the split per group (`machinePlacement: {<node>:
  <replicas>}` in the Application's values; `node_groups[i].placement` in the API). The chart renders exactly that.

The placement rule (`kamajiPlaceGroups`, `pve.PlaceReplicas`, `pve.RemoveReplicas`):

1. **Baseline.** Where the group's workers are now: its stored placement, else the even split of its current count.
   A re-render never moves a running worker; only the difference is placed or removed. An edit that changes no count
   writes the baseline as it is and asks Proxmox nothing (so the first edit of a cluster moved to 0.1.13 stores the
   even split it already runs).
2. **Scale-up and create** place each new replica one at a time on a node that
   - holds the image (an entry of the image string, whose template VMID Proxmox still shows on that node) and is
     online,
   - has the cluster's worker storage active (`proxmox.storage` in the values, fixed at create),
   - has room on that storage for the root disk with the provider's storage headroom (`storageHeadroomPct`),
   - stays under `maxcpu × overcommitCPU` with the flavor's vCPUs,
   - has the flavor's memory free after the memory kept for protected VMs.

   Among those the node with the fewest replicas of this group wins, then the one with the most free memory, then by
   name: a group is spread when several nodes fit. Every pick counts at once, for the next replica and for the next
   group of the same request. On shared storage a disk is taken from every node's view of the pool.

   What counts as taken on a node, the same for the server scheduler (`pve.PickNode`, `pve.CommittedMem`):
   - **Memory:** the larger of (a) the configured memory of every running VM the provider's token may audit and
     (b) the node's own used memory (`mem` of the node's row in `GET /nodes`, which the token reads with
     `PVEAuditor` on `/nodes`). On top of that comes the configured memory of every VM the token may audit that
     does not run (stopped, or cloned and not booted yet: it takes nothing of (b)), of the workers that are decided
     and not built yet (rule 3) and of the picks of this request. (b) is there for the VMs the token may not audit: Proxmox leaves them out of
     `GET /cluster/resources`, and on a managed-services install those are the platform's own machines (pools
     `proxcloud-seed` and `proxcloud-system` carry `NoAccess` for this token by design), on any site also VMs that
     belong to nobody ProxCloud knows.
   - **CPU:** the vCPUs of every VM the token may audit, running or stopped, against `maxcpu × overcommitCPU`.
   - **Known limits.** A VM the token may not audit counts with the memory it uses now, not its configured size,
     and not while it is stopped; it does not count at all on a node where the running visible VMs' configured
     memory is already above the node's used memory (the larger of the two is taken, they are not added). The
     other way round, on a node whose used memory is the larger figure, a running visible VM counts only with
     what it uses now: several creates in a row can still overfill a node that carries hidden VMs, once the
     earlier VMs have booted and use less than their size. A hidden VM's vCPUs are never counted. The node's used
     memory is Proxmox's figure, so it includes the host itself, LXC containers and, on ZFS, the ARC: such a node
     reads less free than the sum of its VMs says, on every site. Cap the ARC (`zfs_arc_max`) if it takes room
     you want placed.
3. **Workers not built yet count.** A clone takes minutes. The workers of every cluster of this Kubernetes provider
   that are decided and that Proxmox does not have yet (placement less the VMs tagged `pc-k8s-<clusterId>` per node)
   count as VMs of that cluster's largest group: its memory and CPU on the node, and its root disk on the cluster's
   worker storage (off that node's pool, or off every node's view of a shared one), which Proxmox still shows free.
   Decisions on one Proxmox provider run one at a time (a lock from the
   snapshot to the Application that records the decision), so two creates in a row do not both take the last free node.
4. **Scale-down** removes each replica from the node whose MachineDeployment has the most replicas that are not ready
   (a stuck machine goes before a healthy one), then the node with the most replicas of the group, then the one with
   the least free memory, then by name. Nothing is checked, so it never fails on Proxmox: the nodes' free memory is
   read for the order only, and when Proxmox does not answer (or the MachineDeployments cannot be read) that
   criterion is skipped. Free memory is counted as in rule 2. ProxCloud decides only how many replicas each node
   loses: **which machine of that node goes is Cluster API's choice** (the MachineDeployment's deletion order,
   `Random` by default; the chart does not set it). On the lab a scale-down removed the older, original worker
   of the node and kept the one added last. "A running worker never moves" holds per node, not per machine.
5. **The image string changes** (an UPGRADE to a version published on other nodes, or the operator edited
   `config.cluster.versions` and the next node group edit picks it up): replicas on nodes that still hold the image
   stay, a new node starts at 0, and the replicas of a node that no longer holds it are placed as new ones, by rule 2.

Refusals are all or nothing and happen before anything is reserved, minted or written:

| Answer | When |
|---|---|
| `409 Node group "G": only K of N new workers fit (flavor F: X vCPU, Y MB, Z GiB disk). Reduce the count, choose a smaller flavor, or contact the operator.` | Not every new replica has a node |
| `409 Node group "G": no node that holds this version's image has the cluster's storage. Contact the operator.` | The cluster's storage is active on none of the image's nodes |
| `409 Node group "G": no node still holds this version's image. Contact the operator.` | The template of the image string is gone from every node that has the cluster's storage (removed or moved after the provider's versions were set) |
| `503 proxmox cluster unavailable` | Nodes, VMs or storage could not be read |
| `409 Another cluster change is being placed on this provider. Try again in a moment.` | The lock was held for 30 s |

What it does not do:

- **Local volumes at scale-down.** Removing a group's last replica on a node leaves the PersistentVolumes on that
  node's storage behind: their pods cannot start elsewhere. The order above empties a node last, the console warns
  when a group is scaled below the number of hosts it runs on, and PersistentVolumes are not read.
- **A node dropped from the image string is pruned at once.** The Application prunes, so that node's
  MachineDeployment and its workers are deleted when the edit syncs, not surge-first; their replacements are being
  built on other nodes at the same time. Room for them is checked before the edit is written. Publishing a version
  on fewer nodes than the current one therefore moves workers at each cluster's next upgrade or node group edit.
- **Nodes whose storage has another name.** One storage name per cluster: a node where it is not active gets no
  replica.
- **No capacity check** for a flavor changed in place (every worker of the group is replaced where it is) or for the
  one surge machine of a rolling upgrade. "Provisioning status" shows the result.
- **Existing clusters are untouched** until an operator sets the provider's `chartVersion` to 0.1.13 and bumps each
  cluster (**Bump** on the provider page, or the tenant's *Apply platform update*). A cluster on an older chart gets
  no `machinePlacement` and no check, exactly as before. After the bump its render is the same until its node groups
  change. Chart 0.1.13 is published and the platform BOM pins it: a managed-services platform installed from this
  release registers its Kubernetes provider with `chartVersion` 0.1.13, and an installer-built site registers the
  chart's source version. A platform installed from an earlier BOM keeps the `chartVersion` it was registered with
  (an apply on it is refused, `bom_unsupported`); the operator sets it on the provider.

**Reaching the platform.** Workers sit on a tenant vnet with no route to the management network. The API-server
endpoint is an address on the services subnet, which tenants reach over their normal egress: MetalLB.
In an EVPN zone that egress is the zone's (Proxmox holds the gateway and does the SNAT). In a **VLAN zone**
Proxmox holds no gateway and does no DHCP or SNAT: the network is a bare VLAN (the API writes and answers
`dhcp: false`, `snat: false`), and the site must provide a router that holds the subnet's first address (the
`gateway` the network shows, which every worker gets as its default route) and routes to the services subnet.
Without that router a worker boots, never reaches its control plane, and the cluster stays `PROGRESSING` with
no ready node. Nothing in a
tenant reaches the PVE API: proxmox-csi's controller runs on the services cluster and calls the CSI gateway.

Earlier releases gave tenant workers a `pve-api` Service on the services subnet that forwarded to the PVE nodes
(`config.cluster.pveCsiUrl`), for a proxmox-csi controller inside the tenant. Stage 60 (`make upgrade` runs it)
deletes it once no cluster's `<id>-csi-config` on the services cluster still holds a token and no cluster Application
has StorageClasses without `proxmox.csi.gateway` (a migration whose Application patch failed), and otherwise names the
clusters to move first ("Moving a cluster onto the CSI gateway" below). By hand:
`kubectl -n proxcloud-system delete svc/pve-api endpointslice/pve-api` on the services cluster. The services cluster's own
proxmox-csi calls the PVE API directly and never used it.

**Node credentials (NodeRestriction).** Kamaji's default admission plugin list leaves out `NodeRestriction`, so on a
tenant API server without it a worker's kubelet credential (`/var/lib/kubelet/pki/kubelet-client-current.pem`, readable
by anything that gets root on the node) can label, taint and cordon any other Node, set `node-restriction.kubernetes.io/`
labels, and read any Secret of the cluster by creating a pod bound to its own node that mounts it. Chart 0.1.6 sets
`KamajiControlPlane.spec.admissionControllers` to Kamaji's list plus `NodeRestriction` (`kamajiControlPlane.admissionControllers`
in the chart values; the portal does not render the key, so the chart default applies). A node can then change only its own
Node and create only mirror pods, which may not reference Secrets. Measured on the lab, Kubernetes 1.35.10, same cluster
before and after a Bump from 0.1.5:

| As a worker's kubelet credential | 0.1.5 | 0.1.6 |
|---|---|---|
| PATCH another Node: label, taint, cordon | allowed (200) | refused: `is not allowed to modify node` |
| create a pod bound to itself mounting a Secret, then read the Secret | allowed, Secret read | refused: `can only create mirror pods`; a mirror pod: `may not reference secrets` |
| label its own Node `node-restriction.kubernetes.io/…` | allowed | refused |

Kubelet heartbeats, Cilium, the proxmox-csi node plugin (PVC attach and mount), MetalLB (L2 announcement) and pods,
`exec` and `logs` kept working, and a worker joining afterwards registered with its node group's label and taint. The
API server rolls in under a minute; `exec` and `logs` answer `No agent available` for about 30 seconds after, until the
konnectivity agents reconnect. A cluster created before 0.1.6 gets the plugin with a **Bump** onto 0.1.6 or later; nothing
on the workers changes.

**Bootstrap ISOs.** CAPMOX writes each new worker's user-data, kubeadm join token included, to
`user-data-<vmid>.iso`, mounts it as a CD, and after the node has joined unmounts and deletes it. The token
lives 15 minutes (CABPK's default) but is renewed until the machine is up, so a node that never joins keeps a
live one. Whoever can read the store can join a node of their own to the cluster. CAPMOX v0.9.2 passes no store
to go-proxmox, which uploads to the first enabled `iso` store in `GET /nodes/<node>/storage` as the token sees
it, and PVE lists stores in Perl hash order: with two iso stores visible the pick changes from call to call.
So each cluster's token sees one iso store, the provider's `config.isoStorage` (default `local`, PVE's stock
directory store on every node), and a create answers 409 when that store is shared (NFS, CIFS, CephFS, ...),
takes no ISO images, when the boot store takes ISO images as well, or when `capmox@pve` itself lacks
`PVEDatastoreAdmin` there. The services cluster's token follows the same rule (installer `services.iso_storage`,
default `local`, checked in preflight). Clusters on `sharedWorkers` use the operator's token: give it
`PVEDatastoreAdmin` on one node-local store and no other iso store.

Clusters created before this release have their token's ISO grant on the old store (`nas-templates` on an
installer site). After upgrading the portal, the sync's sweep moves each one: it grants the new store and
revokes the old, once no `user-data-*.iso` is left on the old store (a node still booting from it would
otherwise keep its file for ever, since CAPMOX deletes it only on stores its token sees). Until then the log
says `bootstrap ISO move waits for a node to join` with the file's name. A node that starts booting from the old
store while its grant is being taken away gets the old store back, and the move waits for it the same way. A
worker that never joins holds every old cluster's move until it is removed. Test connection on the Proxmox provider
warns when `isoStorage` is shared or takes no ISO images: existing clusters only ever move to that store, so a
shared one would otherwise leave them where they are without a word. The user grant comes first; the create and
the move read it off the ACL list the way PVE does, so a grant through a group or a parent path with propagate
counts. Give it on any node, or by stage 10 (`make install` and `make upgrade` run it, writing only what is
missing):

```sh
pveum acl modify /storage/local --users capmox@pve --roles PVEDatastoreAdmin --propagate 0
# check one cluster after the next sync: Datastore.* on local, nothing on the old store
pveum user token permissions capmox@pve <clusterId> --path /storage/local
pveum user token permissions capmox@pve <clusterId> --path /storage/nas-templates
```

**Block volumes.** Why no PVE token goes to a tenant cluster, which earlier releases did with
`config.cluster.csiSharedStores: true`: the plugin's token reaches the tenant cluster, where a cluster-admin can read it, and
**PVE's ACLs do not hold it to the project**: to attach or delete a disk, PVE checks `Datastore.Allocate` on the store and
not the VM that owns the disk, and proxmox-csi cannot work without that privilege, since all its disks are named
`vm-9999-pvc-*`. Scoping the token to the project's pool does not help. Seen on PVE 9.2 with a token on the project's pool
and the store only: it cannot read another project's pool or VM config, but it lists every disk on the store, attaches
another project's PVC or boot disk to its own worker (a pod then read the files), and passes PVE's check to delete one (the
task ran and failed only because that disk was in use). Moving the plugin to the management side would not close this, as a
tenant can create a `PersistentVolume` naming any disk. Hence the gateway below, which checks every call.

**Block volumes through the CSI gateway.** The replacement keeps every PVE credential out of the tenant
cluster. The portal API serves a CSI gateway (`internal/cloud/csigw`, its own TLS listener, `PROXCLOUD_CSI_GATEWAY_ADDR`,
off by default) that the proxmox-csi controller calls as if it were PVE, with a credential per cluster
(`csi@proxcloud!<clusterId>`). The gateway allows only the calls proxmox-csi-plugin v0.22.0 makes and checks each one: a
disk is the cluster's when the gateway's `csiVolume` registry says it created it for that cluster (written before the disk,
so a name is taken once), a VM when it is in the cluster's pool with its `pc-k8s-<clusterId>` tag right now (read on
every call, since PVE reuses a freed vmid at once), a task when the gateway
answered with its UPID. A crafted `volumeHandle`, NodeID or StorageClass is only an input it checks; another cluster's disk
does not even show in a listing, so a delete of it is a no-op and an attach finds nothing. It refuses replication,
`/cluster/nextid`, VM create, migrate and `/access/ticket`, and records every refusal in the audit log. It calls PVE with
one token, `csi@pve!gateway` ([proxmox-permissions.md](proxmox-permissions.md#the-csi-gateway-token)).

A **new** cluster takes this path when the portal has `PROXCLOUD_CSI_GATEWAY_URL` and its CA
([configuration.md](configuration.md)), the compute provider holds the gateway token (`config.csiGatewayTokenId`,
`secret.csiGatewaySecret`) and the workers are the project's (not `sharedWorkers`). Every storage profile is then a
StorageClass, and chart 0.1.7 or later is needed: with an older `chartVersion` on the Kubernetes provider the create
answers 503. Without the gateway a new cluster gets no proxmox-csi at all.

- The create mints the cluster's credential and writes `<id>-csi-gateway` (the controller's `config.yaml` and the
  gateway CA) next to the cloud secret on the services cluster. No CSI token is minted on PVE, and the resource set
  carries only the privileged `csi-proxmox` namespace.
- The chart runs the controller in `pc-<projectId>` on the services cluster (`templates/csi-controller.yaml`): the plugin,
  attacher, provisioner and resizer, each with the Kamaji admin kubeconfig (`admin.svc`, the in-cluster address, so the
  API allowlist does not apply). Leases and storage capacity objects stay in the tenant's `csi-proxmox`; the lease names
  depend on the driver name only. The tenant keeps the node plugin, the CSIDriver and the StorageClasses, with the
  upstream controller at 0 replicas and no config Secret. Snapshots are not offered.
- The tenant's disks are VOLUME rows of the project (`data.volume.k8sCluster`), from the registry, at the size PVE
  reports, and bill like volumes from the disk's creation. The portal refuses to delete or change them itself (409):
  the PersistentVolumeClaim owns them. The sync's reconcile leaves a `creating` row alone for 15 minutes; after that it
  makes the row `ready` if PVE has the disk (a create whose answer was lost) and drops it if not. It closes a row whose
  disk is gone, and takes size and volid from PVE.
- Once a deleted cluster's workers are gone, the release revokes the credential, then deletes every disk the registry
  still holds for it, `Retain` PersistentVolumes included: with the cluster gone nothing can mount them. A disk PVE
  refuses to delete (still in use) keeps the cluster's marker, and the sweep tries again; the cluster's PVE tokens,
  addresses and dataset are released in the meantime.

Deployments: the chart's `api.csiGateway` (its own port, a Service with `externalTrafficPolicy: Local`, a certificate from
a CA made with cert-manager, a NetworkPolicy that admits the port from `allowedCIDRs` only); `make install` in split mode
turns it on with the services nodes (`services.network.nodes`) as the only sources, at the first mgmt node's address.
A Local Service answers only on a node running an API pod, so with three mgmt nodes the installer pins the API to the
first. Existing clusters keep the tenant-side controller until an operator moves them (below).

A PersistentVolume on a node-local store (`zfspool`, `lvmthin`) carries its PVE node as its zone, so its pod runs only on
workers of that node; on a shared store (NFS, RBD) it follows the pod to any worker.

**Moving a cluster onto the CSI gateway.** A cluster made by an earlier release with `csiSharedStores` keeps the
tenant-side controller and its `csi@pve!<id>` token until an operator moves it, one cluster at a time:

```sh
# what it would do; writes nothing
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$API/api/v1/admin/service/<kubernetesProviderId>/k8s-clusters/<clusterId>/csi-gateway?dryRun=true"
# do it
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$API/api/v1/admin/service/<kubernetesProviderId>/k8s-clusters/<clusterId>/csi-gateway"
```

It needs what a new cluster needs (the portal's gateway URL and CA, the gateway token on the compute provider, the
project's own workers) and a Kubernetes provider pinned to chart 0.1.7 or later, which the cluster is bumped onto. The
portal reads the tenant API through the cluster's Kamaji admin kubeconfig, at the control plane's LoadBalancer
address: a cluster whose API allowlist leaves the portal out cannot be moved until it is added.

1. **Adopt.** The portal reads the cluster's PersistentVolumes of driver `csi.proxmox.sinextra.dev`, those of every
   other cluster of the provider, and the services cluster's own (Kamaji etcd, the registry and DBaaS use the same
   `vm-9999-pvc-` names on the same stores; this needs `list persistentvolumes` in `deploy/mgmt-cluster/rbac.yaml`, so
   apply it again on an existing site before migrating: `make upgrade` does not). A PV's disk enters the gateway's
   registry as the cluster's when that PV is the only one naming it, PVE lists it on one of the cluster's stores, it is
   attached to the cluster's workers and to nothing else, and no registry row holds it. Any other PV is refused with its
   reason: another PV names the disk too (a crafted PV, or a disk passed between clusters), the registry holds it for
   another cluster, another VM has it attached, no worker of the cluster has it attached, it is not on PVE, its name is
   not one proxmox-csi gives a volume, it is on another region or store, or some cluster's PVs could not be read (the
   disk could be theirs). A disk nobody else names is not proof it is the cluster's: a disk left by a deleted cluster or
   by another portal on the same PVE, or a services-cluster disk while its pod restarts, is named by nobody. So a PV whose
   pod is not running (scaled to 0, or no pod at all) is refused until the operator has checked the disk and names the
   PV in `?adoptDetached=<pv>,<pv>`; migrate while the cluster's pods run. A refusal stops the migration with 409 and the
   report; `?force=true` goes on without adopting them, and a pod of a refused PV cannot attach afterwards. `-pvc-` disks
   on the cluster's stores that no PV names, no row holds and no VM has attached are listed as `orphans`, never deleted.
   Adopted disks bill from now on (the PVE sync never counted them before) and are not checked against the project's
   quota.
2. **Switch.** The portal mints the cluster's gateway credential, writes `<id>-csi-gateway`, cuts `<id>-csi-config` down
   to the `csi-proxmox` namespace, and in one Application apply pins the provider's chart with the gateway values.
   CAAPH takes the tenant's controller to 0 replicas and the services-side controller takes over the same leases. Pods
   keep their volumes: their I/O does not pass through the controller. On the lab the switch took about 10 seconds.
3. **Clean up.** It deletes the tenant's `csi-proxmox/proxmox-csi-plugin` Secret (the resource set does not delete what
   it no longer carries), then revokes `csi@pve!<id>`. It then reads the PVs again: one the old controller made after
   the first read is listed in `late`, unadopted, and its pod cannot attach; run the migration again for it. The token
   is revoked right after the Application apply, without waiting for ArgoCD to roll out the services-side controller:
   afterwards, check on the services cluster that the cluster's csi controller Deployment is Ready. A cluster being
   deleted is refused.

Running it again is safe: adopted rows stay, a switched cluster is not switched again, and a Secret or token already gone
counts as done. Its CAPI token stays.

For an older cluster not yet moved, a disk is named after its PV alone, so the portal cannot tell whose it is: a PVC still
there when the cluster is deleted leaves its disk on the store, unbilled. Delete PVCs first. A cluster created without the gateway has no proxmox-csi, no CSI token and no
`csi-proxmox` namespace, and `shared-file` is the default StorageClass if it has a dataset. Without a TrueNAS provider on
the project it has no StorageClass at all. A store per project would give real isolation on block volumes. It is not built:
such a store needs a ZFS dataset or RBD namespace per project, and we found no PVE API call that creates one.

**Taking the token back.** Upgrading does nothing to existing clusters made with `csiSharedStores` (or by a version
that always minted the token): they keep the plugin, its StorageClasses and a live token until they are moved or
deleted. A portal with the CSI gateway moves such a cluster onto it, volumes and all ("Moving a cluster onto the CSI
gateway" above). Without the gateway, to take one back, per cluster (`<id>` is the cluster id, `pveum user token list csi@pve` lists them):

1. In the tenant cluster, delete PVCs on those classes and wait for their disks to go. Afterwards the plugin can no longer detach or delete them: a pod that moves answers `proxmox API error 401` on attach, a disk left behind stays on the store unbilled, and a worker replaced while one is attached waits at `WaitingForVolumeDetach`.
2. On a PVE node: `pveum user token remove csi@pve <id>`. This is the step that closes the hole. PVE drops the token's ACLs with it, and a copy left in the tenant cluster is dead.
3. Remove the plugin. This takes chart 0.1.5 or later, because an older chart keeps the plugin with no StorageClass. A cluster from before this setting is on 0.1.4 or older and needs a **Bump** first: with the provider's `chartVersion` at 0.1.5 or later, Bump the cluster on the provider page. Then, on the management cluster:
   ```sh
   kubectl -n argocd patch application <id> --type=json -p '[{"op":"replace","path":"/spec/source/helm/valuesObject/proxmox/csi/storageClasses","value":[]}]'
   kubectl -n pc-<projectId> delete secret <id>-csi-config
   ```
   ArgoCD prunes the plugin's HelmChartProxy, CAAPH uninstalls it and its StorageClasses go, and `shared-file` becomes the default class if the cluster has one. The tenant's `csi-proxmox` namespace and the dead Secret in it stay until the tenant deletes them.

A later delete of the cluster finishes as usual: a token already gone counts as revoked. Keep the `csi@pve` user, even with no token left under it. A release asks PVE to delete that user's token for the cluster, and with the user gone PVE answers 403 to the portal's token. That does not count as revoked, so the release never finishes.

**RWX volumes.** `config.cluster.rwxGiB` gives each new cluster on a project with a TrueNAS provider one NAS dataset
of that size and the `shared-file` StorageClass (csi-driver-nfs, `ReadWriteMany`), the default class unless proxmox-csi
classes are offered. The dataset is an ordinary share of the project named `k8s-<clusterId>`: it bills as a share,
counts against the share quota, can be resized on the Shares page, and its export follows the project's addresses.
PVCs are subdirectories of it named after their PV, so `refquota` caps them together, not one by one, and a new PVC with an old name starts empty. They are mode `0777`, since the NAS squashes root and a non-root pod could not write otherwise. It is destroyed with the cluster, by the sweep after the delete cascade (30 minutes after the cluster was created at the earliest).
For the same reason the CSI controller deletes a PVC's directory as `nobody`: a directory a non-root pod created with mode `755` and filled cannot be removed, the PV stays `Released` with `permission denied` in its events, and its data counts against the `refquota` until the pod's user removes it or the cluster goes. Have such pods use `umask 000`, or empty the volume before deleting the PVC.
Workers reach it over their egress, so the NAS must see them under the project's addresses: no NAT on the way. A subnet
that leaves through its exit node's SNAT, the usual case on EVPN, arrives under a node's address. Where a
NAT cannot be avoided, the TrueNAS provider's `snatSources` keeps its address on the dataset's export, which gives up
isolation between every tenant behind that NAT (see "TrueNAS file shares" in `cloud-integration.md`). The isolation rules
drop tenant traffic to the nodes' management network, so a NAS with only an address there cannot be mounted at all:
`mountHost` must be an address tenants can reach.
Full-tier sites (ceph-csi CephFS) are not built yet.

**A create that fails.** Quota is checked first, so a create over it takes nothing. The marker secret is written before the create takes anything else. A failure releases the addresses, tokens and dataset at once, and if that release fails too (NAS or PVE down), the marker stays and the sweep retries it after the grace window. A release only reaches the NAS named by the cluster's share row, so a broken NAS holding nothing of the cluster blocks nothing. The exception is a cluster on a shared worker pool: its addresses are on every export of the project, so they stay reserved until the NAS takes them off.

**`sharedWorkers`.** For a site that will not give the portal token the right to mint tokens,
`config.cluster.sharedWorkers` (`pool`, `bridge`, `gateway`, `prefix`, `first`, `last`, `capiTokenId`, `csiTokenId`;
`secret.sharedCapiSecret`, `secret.sharedCsiSecret`) puts every cluster on one operator-run pool, network and
credential. **Tenants then share a network and a CSI token any of them can read, and workers neither sync nor bill.**
Their proxmox-csi runs in the tenant with that token and calls the PVE API at `pveApiUrl` (else the Proxmox provider's
`apiUrl`), so the shared bridge must reach it. Lab and evaluation only.

**CAPMOX pitfalls found on PVE 9.2.**

- `not authorized to access endpoint` (upstream issue #513): a manager-level credential
  (`capmox-manager-credentials`, `PROXMOX_URL`) **silently wins over every `ProxmoxCluster.credentialsRef`**
  (`pkg/scope/cluster.go`), so clones run under a token that was never meant for them. Keep the manager
  credential empty: `clusterctl init` with `PROXMOX_URL=""`.
- `no pid returned from agent exec command`: reading cloud-init's status needs `VM.GuestAgent.Unrestricted` on PVE 9,
  which is root in every guest of the pool. The chart sets `checks.skipCloudInitStatus`; the Node joining is the signal.
- A machine whose clone was in flight across a CAPMOX restart can end up "ready" while stopped and unconfigured. Delete
  the Machine and CAPI replaces it.
