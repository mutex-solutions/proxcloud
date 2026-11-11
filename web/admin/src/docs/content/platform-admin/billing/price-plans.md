# Price Plans

A price plan is how ProxCloud knows what to bill. Think of a plan as a bag of pricing rules; each rule prices a single attribute of a single resource type, per unit of time. On a periodic cycle ProxCloud rates every cached cloud resource against whichever plan applies and adds the result to the customer's running bill — pure pay-as-you-go.

## Building a plan

![Current Create price plan dialog filled with synthetic example values](/docs-img/price-plans-create.png)

*Current admin UI captured locally with mock data; no plan was saved.*

Head to **System → Price plans** and create one.

| Field | Meaning |
|---|---|
| Name | An internal label, e.g. `Standard pricing`. |
| Access mode | `PUBLIC` applies to every billing profile automatically; `SCOPED` applies only to the profiles you explicitly assign it to. |
| Enabled | A disabled plan is kept but skipped during rating. |

You can clone an existing plan, which duplicates all of its rules — the fastest way to spin up a customer-specific variant of your standard pricing.

### Which plan a customer gets

A billing profile is rated against the `PUBLIC` plan unless a `SCOPED` plan has been assigned to it, in which case the scoped plan wins. Reach for scoped plans when you have negotiated or segment-specific pricing, and let the public plan cover everyone else.

## Pricing rules

![Current rule list with synthetic instance, volume and public IPv4 pricing](/docs-img/price-plan-rules.png)

*Example rates are illustrative mock data, not a published rate card.*

![Current Add rule dialog showing instance pricing attribute options](/docs-img/price-plan-rule-create.png)

*Local mock illustration; no rule was submitted.*

Open a plan to work on its rules.

Each rule holds:

| Field | Meaning |
|---|---|
| Name | e.g. `Instance vCPU hourly`. |
| Resource type | The billable resource the rule targets (see [Resource Types](/docs/platform-admin/billing/resource-types)). |
| Priced attribute | A numeric or boolean attribute of that resource type. A boolean counts as 0 or 1 — pricing the `existence` attribute charges a flat rate simply for the resource being present. |
| Price | The amount charged per attribute unit, per time unit. |
| Time unit | `minute`, `hour`, or `month`. |

The charge for a single billing tick is:

```
charge = price × attribute value        (per time unit)
```

When a plan carries several rules — or one rule prices several attributes — the amounts add up. To hit a monthly target price, divide by the number of time units in a month: at the defaults a month holds 43,200 minutes or 720 hours, and `month`-based rules are charged once, pro-rated across the remaining days when a resource appears mid-cycle. Those divisors are configurable under **System → Billing configuration → Settings → Time unit limits**.

```
Target: a 2-vCPU / 4 GB instance at ~30 €/month, priced hourly
  vCPU rule:  0.0139 €/h × 2 vCPU × 720 h ≈ 20 €
  RAM rule:   0.0035 €/h × 4 GB  × 720 h ≈ 10 €
```

Rules can also carry filters — applying the price only when a resource attribute meets a condition — plus modifiers, both stored alongside the rule.

## GPU pricing

Instances expose two GPU attributes derived from the flavor's extra specs
(`pci_passthrough:alias`): `gpu_count` (number of devices) and `gpu_model` (the alias,
e.g. `nvidia-a6000` — the same name the provider's GPU capacity tab and project GPU quotas
use). Price GPU flavors per model with one rule each:

```
Rule "GPU — nvidia-a6000"   resource type: instance   time unit: hour
  Filter:  gpu_model  eq  nvidia-a6000
  Price:   gpu_count  →  0.47 $/h per device
```

The GPU rule adds on top of your per-vCPU / per-GB rules, so a GPU flavor's total is
`CPU/RAM components + gpu_count × model rate`. Non-GPU flavors carry `gpu_count = 0` and
are unaffected. A ready-made competitor-benchmarked rate card (RunPod/Lambda for GPU,
DigitalOcean/OVH/AWS for the rest) ships in the repo: `docs/pricing-rate-card.md` +
`deploy/seed/price-plan-seed.json`.

**Guard:** a resource that matches no enabled rule bills **zero, silently**. The cloud
provider's **GPU tab** lists *unpriced flavors* — live flavors matching no enabled public
rule — check it after adding flavors or editing rules.

## Price adjustment rules

On top of per-resource pricing, a plan can hold adjustment rules: tiered surcharges or discounts applied to the already-rated amount. Every tier pairs a starting amount with a modifier (`add` or `subtract`, expressed as a percentage or an absolute value), which lets you model things like volume discounts that deepen as spend climbs. The **Usage** action on an adjustment rule reports how much it has actually added or taken off.
