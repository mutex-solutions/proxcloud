# Connecting an OpenStack Cloud

ProxCloud meters and provisions against one or more OpenStack regions. You register each set of regions as a *cloud provider* in the admin portal; from there ProxCloud reads the regions out of the Keystone service catalog and starts syncing the projects and resources it finds. The reference target is OpenStack 2026.1 deployed with kolla-ansible, but any Keystone v3 cloud will do.

## Before you start

You'll need:

- A reachable Keystone v3 endpoint — this is the Identity URL.
- An admin identity ProxCloud can log in as, either a username/password or an application credential.
- That identity to hold system-level or domain-level admin rights, depending on which authentication mode you choose below.

## Registering the provider

Open **System > Cloud providers** and choose **Add provider**.

### General details

| Field | Description |
|---|---|
| Name | A unique display name; this is what clients see in the portal. |
| Service type | `Cloud` — the only type currently offered. |
| Visibility | `Private` (assigned to chosen billing profiles by hand), `Public` (open to every client), or `Disabled` (hidden — useful during maintenance). |
| Provider | The OpenStack flavor: vanilla OpenStack or Virtuozzo Hybrid Infrastructure. |

### Connection details

| Field | Description |
|---|---|
| Identity URL | The Keystone v3 endpoint, e.g. `https://keystone.example.com:5000/v3`. |
| Authentication mode | How ProxCloud scopes its admin session — explained just below. |

The three authentication modes:

- **System administrator** — the credential carries system-wide admin rights. This is the recommended choice for a cloud you own outright.
- **Domain administrator** — the credential administers a single Keystone domain, and ProxCloud manages only the projects inside it. This is the mode a reseller runs in; see [Reseller Domains](/docs/platform-admin/cloud/reseller-domains).
- **Single shared project** — every client workload is dropped into one shared project. Fine for evaluation, unsuitable for production.

### Admin credentials

| Field | Description |
|---|---|
| Domain name | The Keystone domain the admin user belongs to, e.g. `Default`. |
| Auth type | `Password` or `Application credential`. |
| Username / Password | Needed for the password auth type. |
| Credential ID / Secret | Needed for the application-credential auth type. |
| Shared project ID | Only for single-shared-project mode. |

Prefer application credentials wherever you can: they can be revoked on their own and never put the admin password on the wire.

```bash
# On the OpenStack side, mint an application credential for ProxCloud
openstack application credential create proxcloud-admin --role admin
```

## Discovery happens on save

Saving a new provider triggers an automatic read of its regions and services from the Keystone catalog, so it's ready to use right away. From the saved provider's **Connection** tab you can also:

- **Test connection** — check the stored credentials against Keystone; it reports how many services and projects the account can see.
- **Sync services & regions** — re-read the catalog and rebuild the provider's region list and per-region services whenever you like, for instance after standing up a new OpenStack service.

Discovered regions and services then show up on the **Services** tab, where each service can be enabled or disabled per region.

<!-- screenshot: /docs-img/add-openstack-cloud-connection.png — ProxCloud admin: the provider Connection tab with the Test connection and Sync services & regions buttons -->

## Roles granted to client projects

Open the provider and go to its **Provisioning** tab. List every Keystone role a client user should hold on the projects ProxCloud creates — usually `member`, plus whatever your deployment needs, such as `heat_stack_owner` or an image-upload role. ProxCloud assigns these roles when it bootstraps a project for a client.

## After the provider is saved

- A periodic sync starts against the region. Existing projects, servers, volumes, networks, images, and other resources are pulled into the cloud-resource inventory so billing accrual can begin.
- As billing profiles are activated, new client projects get created in Keystone, tagged by ProxCloud.
- Every service defaults to available; adjust that on the provider's **Services** tab — see [Service Availability](/docs/platform-admin/cloud/service-availability).

## GPU capacity and quotas

The provider's **GPU** tab shows cluster-wide GPU capacity per model, read live from the
Placement API. It expects nova *PCI in placement*: each GPU device is a resource provider
carrying the `COMPUTE_MANAGED_PCI_DEVICE` and `CUSTOM_PCI_GPU` traits plus a
`CUSTOM_PCI_<MODEL>` trait naming the model — that trait becomes the model alias
(`CUSTOM_PCI_NVIDIA_A6000` → `nvidia-a6000`) used everywhere GPU-related: pricing rules
(`gpu_model`), this capacity view, and project quotas. A device counts as *in use* when
its resource provider has any allocation. The same tab lists **unpriced flavors** — live
flavors that match no enabled public price rule and would therefore bill zero.

The dashboard shows the same capacity as a per-model usage bar.

### Per-project GPU quota (two tiers)

- **Tier 1 — ProxCloud**: on a project's **Quota** tab, set per-model device limits
  (`nvidia-a6000 → 4`, or `*` as a catch-all). ProxCloud enforces them when servers are
  created or resized through the client portal/API — a request that would exceed the
  limit is rejected with a clear error. No entry means unlimited.
- **Tier 2 — the cloud itself**: workloads driven directly through Horizon/OpenStack on
  imported projects bypass ProxCloud. Nova's legacy quotas have no GPU class, so cap those
  projects with instance/core quotas in Horizon as a proxy (or evaluate nova unified
  limits for native per-resource-class quotas).
