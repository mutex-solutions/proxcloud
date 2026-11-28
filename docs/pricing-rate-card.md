# Pricing rate card — competitor-mapped (USD, hourly)

Method + concrete rates for the public price plan, benchmarked against published on-demand
prices retrieved **2026-07-08** from official pricing pages (AWS, DigitalOcean, OVHcloud,
Lambda, RunPod). Rates are proposals: apply via the seed payloads in
`deploy/seed/price-plan-seed.json` (or the admin Price plans UI) after sign-off.
Prices are stored in the platform base currency — set base currency **USD** before seeding.

## How the engine models this

One PUBLIC plan, rules keyed `(resourceType, timeUnit)`: every rule is per `hour` except the sandbox ones, which are per
`minute`. The seed (`deploy/seed/price-plan-seed.json`) carries these rules:

| Rule | resourceType | Filter | Priced attribute | Rate |
|---|---|---|---|---|
| CPU component | instance | — | `vcpus` | $0.008 / vCPU / hr |
| RAM component | instance | — | `ram_gb` | $0.004 / GB / hr |
| GPU per model | instance | `gpu_model eq <model>` | `gpu_count` | table below |
| Public egress (`OVERWRITE_TOTAL`) | instance_traffic | — | `outgoing_public_traffic_mb` | tier 1: 0 → 1,048,576 MB (1 TiB) = $0; tier 2: beyond = $0.00001/MB ($0.01/GB) |
| Volume | volume | — | `size` | $0.000137 / GB / hr (≈$0.10/GB-mo) |
| Public IP | floating_ip | — | `existence` | $0.005 / hr |
| Project image | image | — | `size_gb` | $0.000068 / GB / hr (≈$0.05/GB-mo) |
| Server snapshots | server_snapshot | — | `size_gb` | $0.000034 / provisioned GB / snapshot / hr (≈$0.025/GB-mo). **Placeholder, not benchmarked:** the owner sets it. It rates the server's disk size per snapshot, not the snapshot's real usage, so it sits at half the image rate |
| Server protection | server_protection | — | `size_gb` | $0.00014 / replicated GB / hr |
| Managed Kubernetes | kubernetes_cluster | — | `existence` | $0.10 / control plane / hr (workers bill as servers) |
| File share | share | — | `size_gb` | $0.00007 / GB / hr |
| Object storage | bucket | — | `size_gb` | $0.00007 / GB / hr |
| Managed databases | database_cluster | `engine eq <engine>` (8 rules: PostgreSQL, MySQL, MariaDB, Valkey, FerretDB, Kafka, OpenSearch, plus an autoscale surcharge on `autoscale_enabled`) | `vcpus_total`, `ram_gb_total`, `storage_gb_total` | DigitalOcean parity, per engine: see the seed |
| Sandbox CPU + RAM | sandbox (`minute`) | — | `vcpus`, `ram_gb` | $0.00142 / vCPU / min, $0.00018 / GiB / min; the smallest sandbox (1 vCPU, 1 GiB) is $0.00160 / min. The owner's decision of 2026-11-28, modelled on AWS Lambda: see [below](#sandbox-cpu--ram-modelled-on-aws-lambda) |
| Sandbox GPU | sandbox (`minute`) | — | `gpu_count` | $0.0083 / GPU / min. **Placeholder** |
| Sandbox snapshot | sandbox_snapshot | — | `size_gb` | $0.00007 / GB / hr. **Placeholder** |

GPU rules are ADD_TO_TOTAL **on top of** the CPU/RAM component. RunPod/Lambda headline
prices bundle CPU+RAM into the per-GPU rate, so either (a) accept our small premium
(GPU rate + component), or (b) calibrate each GPU rate down by the flavor's component cost
(e.g. an h100 flavor with 16 vCPU / 128 GB carries 16×0.008 + 128×0.004 = $0.64/hr of
component → set the GPU rate to headline − 0.64). The seed uses (a) with rates already
set ~3–5% under RunPod headline; final calibration = operator decision at seeding.

## CPU VM benchmark (per month, 730 h)

| Shape | Ours | DigitalOcean Basic | OVH b3 | AWS m7i |
|---|---|---|---|---|
| 2 vCPU / 4 GB | $23.4 | $24 | — | ($65 c7i.large≈4GB) |
| 2 vCPU / 8 GB | $35.0 | — | $44 (b3-8) | $74 (m7i.large) |
| 4 vCPU / 8 GB | $46.7 | $48 | — | — |
| 8 vCPU / 16 GB | $93.4 | $96 | — | — |
| 16 vCPU / 32 GB | $186.9 | $192 | — | — |

Formula: vCPU $0.008/hr + RAM $0.004/GB/hr. Root disk bundled ($0) like DO; block storage
is billed separately.

## GPU per-model rates ($/GPU/hr, on-demand) — the actual fleet

Aliases below = the cluster's real placement aliases (gpu-info 2026-07-08):
intel-a60 ×1 · nvidia-3080ti ×1 · nvidia-3090 ×6 · nvidia-4090 ×2 · nvidia-a6000 ×18 ·
nvidia-pro-4500 ×4 · nvidia-pro-6000 ×24.

| gpu_model (alias) | Card | Ours | Benchmark | Confidence |
|---|---|---|---|---|
| nvidia-4090 | RTX 4090 24GB | **0.65** | RunPod 0.69 | direct |
| nvidia-3090 | RTX 3090 24GB | **0.43** | RunPod 0.46 (community) | direct |
| nvidia-3080ti | RTX 3080 Ti 12GB | **0.29** | none listed — set just under 3090 | interpolated |
| nvidia-a6000 | RTX A6000 (Ampere) 48GB | **0.47** | RunPod 0.49, Lambda 1.09 | direct |
| nvidia-pro-6000 | RTX PRO 6000 (Blackwell) 96GB | **1.99** | RunPod 2.09 community / 4.00 secure | direct (community-anchored) |
| nvidia-pro-4500 | RTX PRO 4500 (Blackwell) 32GB | **0.69** | none listed — between 4090 and pro-6000 | interpolated |
| intel-a60 | Intel Data Center GPU | **0.15** | no market benchmark | placeholder — operator decision |

pro-6000 is the fleet's flagship (24 devices): anchored to RunPod *community* 2.09 −5%;
RunPod *secure* is 4.00, so there is room to price higher if positioned as secure-grade —
operator call. Interpolated/placeholder rows need sign-off before seeding.

Alias vocabulary = the normalized GPU alias (`nvidia-a6000`): the alias names in the Proxmox
provider's `gpus` map and in the flavor catalog's `gpuModel` — the same names gpu-info capacity
and project GPU quota use. A flavor's model+count come from the flavor catalog
(`internal/platform/pveflavor`), rendered into the extra-specs shape
`pci_passthrough:alias` that `internal/cloud/gpu.go` parses; the seed rule filters use exactly
the aliases above.

## Other resources — benchmarks

- Volume $0.10/GiB-mo (DO volumes); AWS EBS gp3 $0.08/GB-mo; OVH Classic ≈$0.048/GB-mo.
- Public IP: AWS public IPv4 $0.005/hr; DO reserved-unattached $5/mo. Ours $0.005/hr flat.
- Project image: DO snapshots $0.06/GB-mo. Ours half the volume price (a template is a full
  clone at rest, on the capacity it was cloned to). Placeholder pending the operator's sign-off.
- Egress: AWS 100 GB free then $0.09/GB; DO pooled 4–8 TiB then $0.01/GiB; OVH free;
  RunPod/Lambda free. Ours: 1 TiB free per server / month, then $0.01/GB (DO-style,
  undercuts AWS hard, still monetizes heavy egress).

Sources: runpod.io/pricing + runpod.io/gpu-models/{rtx-3090,rtx-pro-6000} ·
lambda.ai/service/gpu-cloud · digitalocean.com/pricing (droplets, load-balancers,
volumes, spaces, bandwidth docs) · aws.amazon.com (ec2 on-demand, ebs, vpc,
elasticloadbalancing, s3) · us.ovhcloud.com/public-cloud/prices.
OVH LB / additional-IP rows are JS-rendered and were secondary-sourced — reverify before
quoting them externally.

## Sandbox CPU + RAM: modelled on AWS Lambda

Decided by the owner on 2026-11-28: a sandbox minute is priced like AWS Lambda and its peers. The rule
`PLACEHOLDER sandbox: CPU + RAM per minute` of the seed holds `vcpus` **0.00142** and `ram_gb` **0.00018** per minute.
Its name keeps the word PLACEHOLDER on purpose (`deploy/seed/seed-operator.py` matches rules by name: a renamed rule
would be added beside the old one on a seeded site, and both would be charged); its values are no placeholders any
more. The sandbox GPU rule and the sandbox snapshot rule are still placeholders. "Lambda" here is AWS Lambda, the
function service; the Lambda of the GPU tables above is a GPU cloud of that name, another company.

**Derivation.** AWS Lambda's list price for x86 in us-east-1 is $0.0000166667 per GB-second of memory with the CPU
tied to the memory, which at the size that gives one vCPU (1,769 MB = 1.7275 GiB) is $0.00172754 per minute. That
amount is split between vCPU and memory at the ratio 8 (`vcpus` = 8 x `ram_gb`), the round middle of what the vendors
that price both publish, and rounded to five decimal places.

**What it is modelled on:** the vendors' public list prices as fetched on 2026-11-28 (USD, on demand, x86, each
vendor's lowest-priced US region, per minute, before any free tier).

| Vendor product | vCPU / min | GiB / min | vCPU : GiB | One vCPU with 1.7275 GiB / min |
|---|---|---|---|---|
| AWS Lambda | one price, on memory | | | 0.00172754 |
| Google Cloud Run, request-based | 0.00144 | 0.00015 | 9.6 | 0.00169913 |
| Azure Container Apps, active | 0.00144 | 0.00018 | 8.0 | 0.00175096 |
| **This rule** | **0.00142** | **0.00018** | 7.9 | **0.00173096** |

For that shape the rule is 0.2% above Lambda, 1.9% above Cloud Run and 1.1% below Container Apps.

**It is the rate that is alike, not the bill of a short run.** The meter bills whole minutes, at least one, from the
moment a sandbox is Ready to its end (`internal/cloud/billingresource/sandbox.go`); Lambda bills by the millisecond
and nothing between two calls. A sandbox that lives 20 seconds pays one whole minute, 0.00160 for the smallest shape,
where a per-second meter at the same rate would charge 0.00053. For use that lasts the rates meet: one hour of one
vCPU with 1.7275 GiB is 0.1039 at this rule's rates and 0.1037 on Lambda. A sandbox cannot have that shape (memory is
whole GiB); the nearest, 1 vCPU with 2 GiB, is 0.1068 an hour.

What else a reader needs:

- **Memory is whole GiB.** A sandbox has whole vCPUs and whole GiB, at least 1 of each: there is no 512 MiB sandbox.
  Per minute: 1 vCPU / 1 GiB **0.00160** (the base minute), 2 vCPU / 1 GiB 0.00302, 2 vCPU / 4 GiB 0.00356.
- **On the invoice a base minute is 0.00.** A sent bill shows two decimal places and rounds every item by itself. The
  sandbox items below 0.005 are summed per project into one line, `Sandbox runs below 0.005 each (<n>)`: four base
  runs of one minute make 0.01, one to three are invoiced nothing. A base sandbox shows 0.01 by itself from 4 minutes.
- **No free tier.** The three vendors above give a monthly allowance, so a small workload that costs 0.00 there is
  billed here from its first minute.
- **No per-request price.** Lambda adds $0.20 per million requests. A rule prices an attribute of a resource per time
  unit and nothing counts requests, so there is no such price here, and the time price was not raised to make up for
  it.
- **The values are in the site's base currency.** A rule has no currency of its own: 0.00142 is 0.00142 of the base
  currency (the seed's is USD), and nothing converts it on a site with another one.
- **An existing site keeps its price.** The seed reaches a site once: the installer runs `seed-operator.py` at its
  first run, and the Platform setup defaults add no sandbox rule at all. Set the two values under **System**,
  **Price plans** (Edit on the rule), or with `PUT /api/v1/admin/price-plan/rule/{id}`. Where no sandbox rule
  exists, add this one with `POST /api/v1/admin/price-plan/rule`: the console's Add rule dialog takes one priced
  attribute per rule. A rule added where none was bills the running billing cycle back, from each sandbox's start
  (from the cycle's start for a sandbox that began before it): add it before sandboxes go live. Change the existing
  rule and do not add a second one: every matching rule adds up.

`internal/platform/pricing/seed_sandbox_test.go` holds the seed to the two values and to the base minute of 0.00160.

## Seeding

1. Base currency USD (admin → Configuration → Billing; create-form on a fresh install).
2. `deploy/seed/price-plan-seed.json` holds the plan + rule bodies:
   `POST /api/v1/admin/price-plan`, then per rule `POST /api/v1/admin/price-plan/rule`
   (inject the returned plan id into each rule's `pricePlanId`).
3. Verify with `GET /admin/service/{id}/unpriced-flavors` — it lists live flavors that
   match NO enabled public rule (those would bill zero). GPU models without a seed rule
   show up there.
