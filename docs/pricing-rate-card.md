# Pricing rate card — competitor-mapped (USD, hourly)

Method + concrete rates for the public price plan, benchmarked against published on-demand
prices retrieved **2026-07-08** from official pricing pages (AWS, DigitalOcean, OVHcloud,
Lambda, RunPod). Rates are proposals: apply via the seed payloads in
`deploy/seed/price-plan-seed.json` (or the admin Price plans UI) after sign-off.
Prices are stored in the platform base currency — set base currency **USD** before seeding.

## How the engine models this

One PUBLIC plan, rules keyed `(resourceType, timeUnit=hour)`:

| Rule | resourceType | Filter | Priced attribute | Rate |
|---|---|---|---|---|
| CPU component | instance | — | `vcpus` | $0.008 / vCPU / hr |
| RAM component | instance | — | `ram_gb` | $0.004 / GB / hr |
| GPU per model | instance | `gpu_model eq <model>` | `gpu_count` | table below |
| Public egress | instance_traffic | — | `outgoing_public_traffic_mb` | tier 1: 0 → 1,048,576 MB (1 TiB) = $0; tier 2: beyond = $0.00001/MB ($0.01/GB) |
| Volume | volume | — | `size` | $0.000137 / GB / hr (≈$0.10/GB-mo) |
| Floating IP | floating_ip | — | `existence` | $0.005 / hr |
| Load balancer | load_balancer | — | `existence` | $0.0165 / hr (≈$12/mo) |

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

Alias vocabulary = the placement trait / pci alias form (`CUSTOM_PCI_NVIDIA_A6000` →
`nvidia-a6000`) — the same names gpu-info capacity and project GPU quota use. A flavor's
model+count derive from `pci_passthrough:alias` extra specs (see `internal/cloud/gpu.go`);
the seed rule filters use exactly the aliases above.

## Other resources — benchmarks

- Volume $0.10/GiB-mo (DO volumes); AWS EBS gp3 $0.08/GB-mo; OVH Classic ≈$0.048/GB-mo.
- Floating IP: AWS public IPv4 $0.005/hr; DO reserved-unattached $5/mo. Ours $0.005/hr flat
  (simple; no attached/unattached split in the billing attributes yet).
- LB: DO from $12/mo; AWS ALB $0.0225/hr + LCU. Ours $0.0165/hr flat.
- Egress: AWS 100 GB free then $0.09/GB; DO pooled 4–8 TiB then $0.01/GiB; OVH free;
  RunPod/Lambda free. Ours: 1 TiB free per server / month, then $0.01/GB (DO-style,
  undercuts AWS hard, still monetizes heavy egress).

Sources: runpod.io/pricing + runpod.io/gpu-models/{rtx-3090,rtx-pro-6000} ·
lambda.ai/service/gpu-cloud · digitalocean.com/pricing (droplets, load-balancers,
volumes, spaces, bandwidth docs) · aws.amazon.com (ec2 on-demand, ebs, vpc,
elasticloadbalancing, s3) · us.ovhcloud.com/public-cloud/prices.
OVH LB / additional-IP rows are JS-rendered and were secondary-sourced — reverify before
quoting them externally.

## Seeding

1. Base currency USD (admin → Configuration → Billing; create-form on a fresh install).
2. `deploy/seed/price-plan-seed.json` holds the plan + rule bodies:
   `POST /api/v1/admin/price-plan`, then per rule `POST /api/v1/admin/price-plan/rule`
   (inject the returned plan id into each rule's `pricePlanId`).
3. Verify with `GET /admin/service/{id}/unpriced-flavors` — it lists live flavors that
   match NO enabled public rule (those would bill zero). GPU models without a seed rule
   show up there.
