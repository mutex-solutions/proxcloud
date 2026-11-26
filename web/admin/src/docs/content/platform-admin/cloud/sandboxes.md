# Functions on the sandbox provider

Serverless functions run on the machines that run sandboxes. A tenant deploys a container image and calls it at `https://<name>-<6 hex>.<sandbox domain>`: traffic starts it, load scales it out to as many instances as you allow, and it stops after it has been idle. A function is billed per minute while at least one instance runs. This release serves functions from images, on the sandbox proxy's address only: no build from a repository, no public endpoint, no hostname of the tenant's own and no GPU.

Functions exist only on a sandbox provider that a [managed services](/docs/platform-admin/managed-services) installation built, and only once the platform's BOM pins sandbox chart 0.2.0 or newer. Under an older BOM the console shows nothing of this page: no **Functions** box in the settings, no **Functions** group on the provider, no function quota on a project, and the API refuses the settings that name them.

Not verified on a lab yet.

## Switching functions on

Three steps. Each is refused until the one before it is done.

1. **Managed services → Settings → Sandboxes.** Choose where sandboxes run if you have not. Read **What functions add**, tick **Functions**, fill in the **Portal URL**, save, then **Install** or **Apply this release**. The run builds the function side: the sandbox proxy's function mode, the identities and policies of the `fn-<project>` namespaces, and the token the proxy shows the portal. Tenants see nothing yet.
2. **Check the release gates**, on the Ready card or on the provider's page. The answer has a second list, **Function gates**.
3. **Cloud providers → the sandbox provider → Functions → Offer serverless functions on this cluster**, and save. From then on tenants with this provider see **Functions**.

## What functions add to what sandboxes give up

The settings print this in full before the box, for the placement you chose. In short:

- **Other tenants' secrets on the same machine.** A kubelet reads the Secrets of the pods on its machine. Code that escapes gVisor on a sandbox machine therefore holds the environment secrets and the registry passwords of every function that runs there. Sandboxes put no tenant Secret on those machines; functions do. With a **shared node group** that machine is a machine of the platform cluster, so this comes on top of everything the shared text already lists. A **separate sandbox cluster** keeps the escape away from the platform cluster, but not from other tenants' functions on the same machine.
- **The cluster's copy is not encrypted.** Each function's environment secrets and registry login are a plain Kubernetes Secret in etcd, and this release sets up no encryption for it. With a separate cluster that etcd is the Kamaji datastore on the platform cluster. The portal's own copy is sealed.
- **Callers cost the tenant money.** A function's address has no caller authentication. Anyone who reaches the proxy address keeps a function awake and, when it scales on load, at its highest instance count, and the tenant is billed for every minute. A new function starts with at most 1 instance; the provider allows up to 5 unless you change it. A tenant who needs callers checked does so in the function, for example on a header.
- **Images are pulled by the machines.** containerd on a sandbox machine contacts the registry host a tenant names, outside the network rules that hold the pods. The host must be a DNS name.
- **One domain for every tenant.** Every function answers under the sandbox domain: serve nothing under it that sets a session cookie for the whole domain.

## The settings

| Field | What it is |
|---|---|
| **Functions** | The box under the text. Off is where every installation starts. It is cleared when you change the placement of an installation that is not built yet, because each placement has its own text. |
| **Portal URL** | Where the sandbox proxy reaches this ProxCloud over HTTPS, to start a stopped function and to report traffic: `https://` and a host, with a port if needed, no path. With a separate cluster the sandbox machines' firewall is opened to this address. |
| **Portal CA** | The certificate the proxy trusts for that URL, as PEM. Empty when the portal's certificate is publicly trusted. |

A function pod cannot reach a portal on a private address. A portal on a public address it reaches as the internet does; the two routes the proxy calls answer only to the proxy's token. That token can start any function of the provider and report traffic for it, so a holder can keep functions running at their tenants' cost. It cannot read or change a function. An apply with the box ticked makes a new one and rolls the proxy.

Taking the tick away is refused while a function exists, and sandboxes cannot be switched off while it is set: switch functions off on the provider first (below), let their owners delete them or delete the projects, then take the tick away and apply.

## The function gates

They decide one thing: whether **Offer serverless functions on this cluster** is accepted. They never keep the provider from going live for sandboxes.

| Gate | It holds when |
|---|---|
| **Job scheduler** | The scheduler is on. It stops idle functions every 15 seconds and settles record and cluster every minute; without it nothing would stop a function and nothing would be billed. |
| **Sealing key for function secrets** | A sealing key is configured, so the portal stores function secrets sealed. |
| **A sandbox-proxy replica reported in the last 90 seconds** | The proxy runs in its function mode and reaches the portal URL with the token. After an apply with the box ticked, wait for the proxy to roll. |
| **Billing is not the external provider** | Usage is rated by this ProxCloud. With the external billing provider functions are refused in this release. |
| **Price rule for functions** | A warning, which refuses nothing: no enabled rule prices the resource type `function`, so functions run and bill nothing. |

## The Functions group

| Field | What it does | Default |
|---|---|---|
| Offer serverless functions on this cluster | Switches functions on for the tenants of this provider. Off: the tenant console hides Functions and every function is stopped. | Off |
| Minimum instances (every function) | **0**: a function stops when idle and costs nothing while stopped. **1**: every function always keeps one instance and is billed all the time. | 0 |
| Highest minimum a tenant may set | **0**: a tenant cannot keep a function running. **1**: a tenant may set a function to keep one instance; that function is billed all the time. | 0 |
| Default idle time (s) | How long a function stays up after its last request before it stops, unless it sets its own. | 300 |
| Shortest / Longest idle time a function may set (s) | The range a tenant chooses from. The shortest cannot be under 60: the proxy reports traffic every few seconds. | 60 / 3600 |
| Max CPU per instance (millicores) | At least 250. | 2000 |
| Max memory per instance (Mi) | At least 256. | 4096 |
| Most instances a function may scale to | 1 to 100. **1** turns scaling on load off. A new function starts with at most 1 until its owner raises it. | 5 |
| Functions per project, whatever its quota | An upper bound over every project's quota. | 20 |
| Default function quota of a project | Functions, vCPU and RAM a project's functions may hold together, when the project has no function quota of its own. | 20, 8, 16384 MB |

An empty number field takes the default. The form refuses a block the API would store and then not serve (a block that is on and not valid makes functions unavailable on the provider, while its sandboxes keep working). Functions need the provider's **Sandbox domain**. Each instance gets 1 GiB of ephemeral storage, fixed in this release.

Every running instance is billed, and each instance takes its CPU and memory from the sandbox machines: sandboxes and functions share them. A stopped function takes nothing, so a start can find no room; the caller then gets `503` and the function is stopped again within two minutes, at no cost.

## Quota

A project's function quota is a field of its own (**Projects → a project → Quota → Function quota**), apart from the resource quota: functions, vCPUs and RAM. A blank field falls back to the provider's default, then to the built-in one; it is never unlimited. Usage is counted as reserved: the CPU and memory of every function at its highest instance count, running or not, so a start is never refused by a quota that a create passed.

## Switching functions off

Switching **Offer serverless functions on this cluster** off, or setting the provider **DISABLED**, stops every function of the provider within 15 seconds, also the ones with traffic or a minimum of one. Nothing runs and nothing is billed. The functions, their deployments and their addresses are kept: tenants can still read and delete them, and everything else answers `403` until you switch functions on again.

**Before you roll the portal back to a release without functions**, switch functions off on the provider and wait until no function has an instance. A portal of an older release does not stop function pods, does not bill them, and cannot finish deleting a project that holds a function.

## Billing

Functions bill as the resource type `function`, with **minute** rules on `vcpus` and `ram_gb` (per instance, times the instances that run). See [Resource types](/docs/platform-admin/billing/resource-types).

- A function is charged for the minutes it has an instance, rounded up per running interval, and nothing while it is stopped. The idle time before it stops is charged. A start that never served a request (the image cannot be pulled, no room, a crash loop) is charged nothing.
- Without a rule functions run and bill nothing. A rule you add later bills a function that is running back to the start of the billing cycle or to its own start, whichever is later.
- Do not filter a function rule on `status`.
- Time nobody observed (the scheduler off, the API down) is written off. A cluster that cannot be reached is billed at its last known count for up to 30 minutes, then written off.
