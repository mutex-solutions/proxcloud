# Savings Plans

Savings plans swap commitment for a discount: a customer promises a minimum monthly spend over a fixed term and, in return, gets a percentage off. You define the plans and their discount tiers; customers — or you, acting for them — sign contracts against those plans.

## How the model behaves

- The customer commits to a monthly amount (say 500/month) for the plan's term.
- Usage the plan covers is discounted at the tier rate that matches the committed amount.
- Spend **over** the commitment is charged at ordinary price-plan rates.
- Spend **under** the commitment doesn't lower the charge — the committed minimum is owed regardless. That's the deal.
- Contracts paid **upfront** (the whole term in advance) can earn deeper discounts than pay-as-you-go ("no upfront") contracts.

## Defining a plan

Go to **System → Savings plans** and create one.

![Current Create savings plan dialog with synthetic target resources and duration](/docs-img/savings-plan-targets.png)

*Current UI, synthetic local mock example; not a live offer or saved plan.*

| Field | Meaning |
|---|---|
| Name | Customer-visible, e.g. `Compute savings`. |
| Target resources | Which resource types (with optional filters) the discount covers — scope a plan to compute, storage, or any subset; multiple targets are allowed. |
| Duration (months) | The contract term for this schedule, e.g. `12` or `24`. |
| Discount tiers | Rows of *committed amount from* → *discount %*. |
| Tiers apply to upfront-paid contracts | Whether this tier set is the **upfront** or the **no-upfront** schedule for that duration. |

Tiers climb with commitment — a bigger committed amount reaches a higher tier and earns a larger discount:

![Current savings plan dialog with two synthetic commitment discount tiers](/docs-img/savings-plan-discount-tiers.png)

*Local mock illustration only; no contract or pricing change was submitted.*


```
Duration: 12 months, no upfront
  ≥ 100 / month   →  5 %
  ≥ 500 / month   → 10 %
  ≥ 1000 / month  → 15 %

Duration: 12 months, paid upfront
  ≥ 100 / month   →  8 %
  ≥ 500 / month   → 14 %
  ≥ 1000 / month  → 20 %
```

A single plan can hold several schedules (12-month, 24-month, and so on), each with its own tier sets. The plan list summarizes them, e.g. `12 mo: up to 10%`.

## Contracts

A contract binds a billing profile to a plan. From the same page you can create one for a customer:

| Field | Meaning |
|---|---|
| Billing profile | The customer entering the commitment. |
| Plan / Duration | Must be one of the plan's defined schedules. |
| Committed / month | The monthly commitment — this sets the tier and therefore the discount rate. |
| Paid upfront | Selects the upfront tier set (and payment model). |
| Start | `CURRENT_MONTH` or `NEXT_MONTH`. |

Active contracts are listed alongside their committed amounts, so you can see who's on which plan.

A contract you create here is **ACTIVE** right away, and any upfront money is yours to collect: use it for a customer who paid you outside the portal. The contract records you as the one answering for that payment. An upfront contract a customer buys in the console stays **PENDING_PAYMENT** and discounts nothing until their deposit for it settles.

If a customer pays such a contract outside the portal, open the plan, find the contract and click **Record payment** with the bank reference. It goes active the way a settled deposit would, and if its start month has gone by, its term starts in the current month. API: `POST /api/v1/admin/savings-contracts/{id}/record-payment` with `{"reference": "..."}`.

### Upfront contracts from before payments were enforced

A customer's upfront purchase used to go **ACTIVE** at once, with nothing paid. At every start the API moves any upfront contract that is **ACTIVE** with no deposit and no operator on record to **PENDING_PAYMENT**, priced, and logs the contract ids. Its discount stops from the next charge, and what it already discounted stays on those bills. The customer sees it waiting for payment and can pay or cancel it. If they did pay you, click **Record payment**. Editing such a contract back to ACTIVE is refused.

A deposit that paid a contract cannot be refunded while the contract is active. Cancel the contract first, knowing any discount it already gave stays on the bills it is on.

## Expiry reminders

So a contract's end doesn't take customers by surprise, enable **Savings contract expiry notifications** under **System → Billing configuration → Settings**. Enter the reminder offsets as comma-separated days before expiry, e.g. `30, 14, 7` — the customer receives an email at each checkpoint. Renewal means a new contract; expired contracts do not renew on their own.
