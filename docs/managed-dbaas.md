# Managed Databases (DBaaS) — operator guide

ProxCloud provisions customer databases as **ArgoCD Applications on a dedicated, ops-built DB
Kubernetes cluster** — the managed-Kubernetes (kamaji) delivery pattern pointed at a different
cluster. One database = one Application rendering the `database-cluster` chart
(`deploy/charts/database-cluster`), which emits the engine operator CR, a tenant-facing Octavia
LoadBalancer Service and a NetworkPolicy. ProxCloud stays stateless about desired spec: the
Application's `helm.valuesObject` is the state store, status is read back off Application
health + the LB Service.

```mermaid
flowchart LR
    proxcloud["ProxCloud"]
    subgraph cloud["OpenStack cloud"]
        subgraph dbc["DB cluster (ops-built, OpenStack VMs)<br/>ArgoCD + AppProject proxcloud-dbaas<br/>CNPG · Percona PS · mariadb-op · valkey-op<br/>OCCM + cinder-csi"]
            app["Application std-&lt;id&gt;<br/>(chart database-cluster)"]
            pods["DB pods + PVCs<br/>ns st-&lt;projectId&gt;"]
            lb["Service std-&lt;id&gt;-lb<br/>(LoadBalancer)"]
        end
        octavia["Octavia amphora<br/>(dbaas keystone project)"]
        vip["VIP on the CUSTOMER's<br/>VPC subnet"]
    end
    client["Customer app<br/>(tenant VM)"]
    proxcloud -- "Application CRs<br/>(kubeconfig, SSA)" --> app
    proxcloud -- "neutron RBAC<br/>access_as_shared" --> vip
    app --> pods
    app --> lb
    lb -- "OCCM" --> octavia
    octavia --> vip
    client -- "5432/3306/6379/27017" --> vip
```

Engines (all five ship in the chart; valkey is beta-gated):

| Engine | Operator | HA semantics | Client port |
|---|---|---|---|
| `postgresql` | CloudNativePG ≥ 1.29.1 | `instances` 1/2/3, operator failover | 5432 |
| `mysql` | Percona Operator for MySQL (PS) | group replication, size 1 or 3 | 3306 |
| `mariadb` | mariadb-operator 26.x | replication + chart-owned HAProxy | 3306 |
| `valkey` | valkey-operator (pre-GA, **beta**) | primary + replicas | 6379 |
| `ferretdb` | FerretDB v2 over a CNPG backend | CNPG instances + stateless frontends | 27017 |
| `opensearch` | opensearch-k8s-operator 3.0.2 | N same-size nodes (all roles), 1 or 3 | 9200 (HTTPS) |
| `kafka` | Strimzi 1.1.0 (KRaft, Kafka 4.2.0–4.3.0) | dual-role KRaft pool, 3 brokers | 9094 (SASL/SCRAM) |

Unlike managed-k8s, the compute/storage under a database is **ops-owned** — nothing lands in
the customer tenant except the LB VIP. Billing therefore carries the full price on the
`database_cluster` resource type (pre-multiplied totals `vcpus_total`/`ram_gb_total`/
`storage_gb_total`). While the endpoint is absent the charge is **deferred, not waived**:
once the Octavia VIP appears the engine back-bills the current cycle's elapsed hours from
creation.

## Cluster prerequisites (ops, out-of-band)

Everything in `deploy/dbaas-cluster/` — apply order, RBAC + kubeconfig recipe, AppProject
`proxcloud-dbaas`, `argocd-cm` Lua health checks for the engine CRDs, the pinned operator install
list, and the Octavia/amphora quota math live there. Assumed pre-existing on the cluster:
openstack-cloud-controller-manager (Octavia LBs), cinder-csi + a default StorageClass, ArgoCD
(self-syncing) in `argocd`.

The Lua health checks are load-bearing: the LB Service's built-in health keeps an Application
`Progressing` until the Octavia VIP is programmed, so ProxCloud never reports READY without an
endpoint — and a missing engine-CRD check would leave Applications Healthy while the database
is degraded.

## Provider registration

Admin UI → System → Cloud providers → Add provider → **Database (DBaaS)**, or `POST
/api/v1/admin/service` with the doc shape documented in
`internal/platform/externalservice/dbaas.go` (`config.provider = "dbaas"`). Fields that matter:

- `secret.kubeconfig` — the DB cluster kubeconfig from `deploy/dbaas-cluster/rbac.yaml`'s
  recipe (token or client-cert only; encrypted at rest, stripped from admin reads).
- `config.argocd.*` — namespace/project/chartRepo/`chartVersion` (pinned, never latest).
- `config.database.osServiceId` / `osProjectId` — the OpenStack service the DB cluster's nodes
  live on and the **dbaas keystone project id** there: the neutron-RBAC `target_tenant` every
  customer network is shared with.
- `config.database.memberSubnetId` — the DB-cluster node subnet
  (`loadbalancer.openstack.org/member-subnet-id` on every LB).
- `config.database.engines` — the curated catalog (versions / default / replicas / `beta`).
- `config.services.database.<region> = true` — un-hides the client Databases surface.

Seed example: `deploy/seed/external-service-dev.json` (`svc-dbaas-dev`).

## Networking (why neutron RBAC)

The OCCM on the DB cluster authenticates as the ops-owned dbaas keystone project; for it to
plant an Octavia VIP on a customer subnet, the customer network must be visible to that
project. At database create ProxCloud verifies the customer owns the network/subnet, then creates
a neutron RBAC policy (`access_as_shared`, target = `osProjectId`) with the project's OpenStack
admin creds re-scoped to the tenant. The share is recorded as annotations on a per-database
marker secret (`<id>-net-share`) on the DB cluster — the only durable revocation record.
`loadBalancerSourceRanges` (Octavia ACLs) enforce the customer's allowed CIDRs; the chart's
NetworkPolicy deliberately does NOT mirror them (post-LB source IPs are amphora/node IPs).

## Lifecycle

```mermaid
sequenceDiagram
    participant C as Customer
    participant S as ProxCloud
    participant N as Neutron (customer tenant)
    participant A as ArgoCD (DB cluster)
    participant OP as Engine operator
    participant O as Octavia (via OCCM)
    C->>S: Create database (engine, size, VPC network/subnet)
    S->>S: Prove network/subnet belong to the tenant
    S->>A: pc-project namespace + default-deny NP + net-share marker Secret
    S->>N: RBAC access_as_shared → dbaas keystone project
    S->>A: (mariadb/valkey) auth Secret + Application CR (pinned chart, full values)
    A->>OP: Render + sync chart (engine CR, LB Service, NetworkPolicy)
    OP-->>A: Pods ready (Lua health)
    O-->>A: VIP programmed on the tenant subnet (Service health)
    S-->>C: Status READY + endpoint (sync reads Application + LB Service)
    C->>S: GET_CONNECTION_INFO
    S->>A: Read operator secret + LB ingress on demand (never stored)
    C->>S: Delete database
    S->>A: Delete Application (resources-finalizer cascades, LB torn down)
    S->>N: Sweep revokes the RBAC share once the LB port is gone
```

- **Create** — client `POST /project/{id}/cloud` `{type: DATABASE_CLUSTER, data: {name,
  engine, version, replicas, cpu, memoryGiB, storageGiB, networkId, subnetId, allowedCidrs,
  beta?}}`. Order: namespace (+ its `proxcloud-default-deny` NetworkPolicy) → net-share marker →
  neutron RBAC share → (mariadb/valkey) proxcloud-owned `<id>-auth` secret → Application —
  marker BEFORE share, so a crash mid-create always leaves a record the sweep converges on.
  Status PENDING → PROGRESSING → READY as ArgoCD converges and the VIP is programmed
  (minutes; the charge is deferred until the endpoint exists, then back-billed).
- **Actions** — `GET_CONNECTION_INFO` (secret + LB read on demand, never stored),
  `RESIZE {cpu,memoryGiB}`, `RESIZE_STORAGE {storageGiB}` (grow-only), `SCALE_REPLICAS`,
  `UPGRADE {version}` (catalog-gated, upward-only vs live values; the operator rolls the pods
  onto the new engine image, the endpoint never changes — off for valkey [operator unpinned]
  and ferretdb [frontend/DocumentDB images are a pinned matched pair]), `RESTART`, `RESET_PASSWORD`
  (returned once), `SET_ALLOWED_CIDRS`, `SET_SSO` (opensearch only: Dashboards OIDC against a
  **public** IdP client — no client secret is ever stored; API-side securityconfig wiring is a
  live-drill item). Engine-gated via `dbaas.Capabilities` — verify each mechanism live before
  widening the map.
- **Delete** — Application delete only; the ArgoCD resources-finalizer cascades the chart, the
  LB Service delete tears the Octavia LB (and its tenant-subnet port) down. The periodic sweep
  (`syncjob.sweepDbaasOrphans`) then revokes the network share (skipped while a live sibling
  database rides the same network), reaps the marker/auth secrets and GCs the emptied
  namespace. A neutron 409 while the amphora winds down is the normal first-pass outcome —
  fail-closed retry, never force-delete ports.
- **Project teardown** — dbaas rows are swept before the tenant sweep, and keystone tenant
  deletion is DEFERRED while any database remnant is still finalizing (the LB port / network
  share would wedge it). Re-run teardown after the sweep reports clean.

## Pricing (DigitalOcean parity)

The seed price plan carries **one `database_cluster` rule per engine** (filter `engine eq …`),
rated on the pre-multiplied totals and derived from DigitalOcean's managed-databases price
list (monthly/730h; derivation table in `deploy/seed/price-plan-seed.json` `_readme`):

| Engine | vCPU/h | GB-RAM/h | GB-disk/h | DO anchor |
|---|---|---|---|---|
| postgresql, mysql, mariadb, ferretdb | $0.002740 | $0.015068 | $0.000295 | $15.15/mo (1c/1GB/10GB), exact at small tiers |
| valkey | $0.002945 | $0.014658 | $0.000295 | $15/mo (1c/1GB/10GB) |
| opensearch | $0.002740 | $0.006164 | $0.000295 | exact on all three DO tiers |
| kafka | — | $0.028767 | $0.000295 | DO prices the 3-broker cluster; RAM-rated |

Example: a PostgreSQL 2 vCPU / 4 GB / 60 GB single node rates to $60.90/mo (DO: $60.90);
Kafka with three 2c/2GB brokers at 40 GB each rates to about $149/mo (DO: $148.80).
Re-derive when DO reprices.

## HA and write-node failover (why the endpoint never moves)

The customer-facing address is the Octavia VIP — a fixed IP on the tenant subnet. Failover
never changes it; the "flip to the new write node" happens INSIDE the cluster, behind the LB:

| Engine | Write-path routing behind the VIP |
|---|---|
| postgresql | LB Service selects `cnpg.io/instanceRole: primary`; CNPG re-labels pods on failover/switchover, so the Service endpoints flip to the new primary (verify on 1.29; fallback = CNPG `managed.services.additional` selectorType `rw`) |
| mysql | LB targets the Percona operator's HAProxy pods; HAProxy follows the group-replication primary election |
| mariadb | LB targets the chart-owned HAProxy, whose backend is the operator's `<id>-primary` Service — the operator re-points its endpoints on switchover/failover |
| ferretdb | LB targets the stateless frontends, which talk to the CNPG `-pg-rw` Service (operator-managed) |
| valkey | beta — routing unverified until the operator CRD is pinned |
| opensearch | LB targets all nodes (every node coordinates); cluster re-elects internally |
| kafka | Strimzi-created LBs: ONE bootstrap VIP + one VIP per broker (advertised addresses) — N+1 Octavia LBs per cluster, mind the amphora quota; partition leadership moves internally |

Clients see a few seconds of connection resets during a failover, reconnect to the SAME
host:port, and land on the new primary. No floating-IP dance, no DNS TTL.

## Vertical autoscale (opt-in, surcharge-priced)

`SET_AUTOSCALE {enabled, maxCpu, maxMemoryGiB, maxStorageGiB}` — available on every engine.
Design: **the VPA is the brain, proxcloud is the hand**. The chart renders a
VerticalPodAutoscaler in `updateMode: Off` (recommendation only — a VPA that evicts database
pods on its own schedule is a data-safety hazard); each sync cycle proxcloud reads the
recommendation, clamps it into the customer's ceilings and applies it through the SAME values
patch RESIZE uses, so the operator performs its own safe rolling change. Disk rides the same
tick: when any of the database's PVCs passes 80% full (kubelet volume-stats via the provider's
Prometheus metrics config), the volume grows 20% (grow-only), up to `maxStorageGiB`.

Scale-UP only by design — automatic shrink flaps and is where databases lose caches; scaling
down stays a deliberate customer RESIZE. Billing: the scaled-to size bills through the normal
declared-size totals (the tick raises the declared values), plus a flat surcharge
(`autoscale_enabled`, $0.0137/h ≈ $10/mo per database) while enabled. Prerequisites on the DB
cluster: the VPA recommender, and Prometheus metrics config on the provider for the disk leg —
see `deploy/dbaas-cluster/README.md`.

## Chart pin / platform update

Databases keep their chart pin when the provider's `chartVersion` moves. Operator override:
`GET /api/v1/admin/service/{id}/db-clusters` lists pins; `POST .../db-clusters/bump-chart`
(all) or `POST .../db-clusters/{clusterId}/bump-chart` (one) re-pins onto the provider's
current version.

## Connection secrets (contract, pinned by TestConnectionSecretContract)

| Engine | Secret | Exposed account |
|---|---|---|
| postgresql | `<id>-app` (CNPG-minted) | `username`/`password`/`dbname` keys |
| ferretdb | `<id>-pg-app` (CNPG backend `<id>-pg`) | same keys, Mongo wire |
| mysql | `<id>-secrets` (operator-minted) | `root` under key `root` (PS has no declarative app users) |
| mariadb | `<id>-auth` (**proxcloud-provisioned**) | user `app`, database `app` |
| valkey | `<id>-auth` (**proxcloud-provisioned**) | AUTH only |
| opensearch | `<id>-admin-password` (operator-minted, default securityconfig) | `username`/`password` keys |
| kafka | `<id>-auth` (**proxcloud-provisioned**, KafkaUser BYO-password) | SASL user `<id>-app`, SCRAM-SHA-512 |

The proxcloud-provisioned secrets live OUTSIDE the Application on purpose: a chart-generated
password would re-roll on every ArgoCD render, and values must never carry secrets (the
Application CR is readable by anyone with argocd read).

## Verify at the live drill (flags embedded in the chart/manifests)

CNPG `instanceRole: primary` LB selector + restart annotation; Percona served CRD version,
secret keys, haproxy labels; mariadb `podMetadata` passthrough + `-primary` Service naming;
the whole valkey CRD (beta gate stays closed until pinned); FerretDB healthz + CNPG
DocumentDB bootstrap; engineVersion→image maps; Lua status value sets; PodMonitor ports.

## Troubleshooting

- **Endpoint stuck pending** — `kubectl -n pc-<project> get svc <id>-lb`; empty
  `EXTERNAL-IP` → OCCM events (amphora quota in the dbaas project, member-subnet ports,
  neutron RBAC missing).
- **Application Degraded on first create (ferretdb)** — frontend crash-loops until the CNPG
  backend accepts connections; the startup probe absorbs the normal window.
- **Sweep logs "revoke network share: 409"** — expected while the amphora winds down; the next
  cycle retries. Persistent 409 = a foreign port of the dbaas project on the tenant network.
- **`not managed by proxcloud — refusing`** — the object lacks the
  `app.kubernetes.io/managed-by: proxcloud` label; pre-existing/hand-made objects are invisible
  and untouchable by design.
