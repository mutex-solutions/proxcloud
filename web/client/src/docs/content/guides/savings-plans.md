# Savings Plans

A savings plan is a committed-use discount. You pledge to spend a set amount each month on eligible resources, and in exchange every bill discounts the usage that matches the plan by a percentage. Committing for longer — and paying more of it upfront — reaches a higher discount tier. The conceptual mechanics behind the discount are covered in [How Metering and Billing Work](/docs/concepts/billing-and-metering); this guide is the hands-on version.

Find them under **Billing → Savings plans**.

## How the discount is calculated

- Each plan your operator publishes covers a **resource scope** — a specific plan, or any resource — and offers one or more **duration** choices (say 12 or 24 months).
- Inside a duration, the discount is **tiered by your monthly commitment**: pledge more per month and you unlock a higher percentage.
- Two tier schedules can sit side by side — **no upfront** (you just pay monthly) and **upfront** (you pay the whole commitment in advance for a deeper discount).
- On every monthly bill, matching usage is discounted at your contract's rate. The commitment acts as a floor: if your matching usage in a month comes in under the committed amount, you're still billed the committed minimum.
- Upfront payments are drawn down against your monthly bills, not charged a second time.

## Buying a plan

1. Open **Billing → Savings plans**.
2. Choose a plan from the catalog and read its discount schedule.
3. Select a **duration** (in months) from the dropdown.
4. Enter your **Monthly committed amount** in your billing currency.
5. Set the start: **This month** or **Next month**.
6. Optionally tick **Pay upfront** for the upfront tiers — bear in mind a paid upfront contract can't be cancelled.
7. Click **Purchase**. The contract shows up in your contracts table: **ACTIVE** without upfront payment, **PENDING_PAYMENT** with it.
8. For an upfront contract, open its menu and pay by card or bank transfer. It costs the whole term's commitment less its discount, and it starts discounting once the money has arrived. If its start month has gone by then, the term starts in the current month, so you get every month you paid for. Until you pay you can still cancel it. An upfront contract bought before payments were enforced shows as **PENDING_PAYMENT** too, and gives no discount until it is paid. If you already paid your provider, ask them to record the payment.

*The purchase-form illustration is deferred: this page imports third-party payment SDKs and is excluded from the strict localhost-only screenshot run. No payment or contract mutation is performed for documentation capture.*

## Managing your contracts

The same page lists your contracts with their plan name, duration, monthly commitment, discount rate, and end date.

- **Extend contract** — pushes the end date out by the contract's own duration (a 12-month contract gains another 12 months). Upfront contracts can't be extended, since the extra months would not be paid for: buy a new one when the term ends.
- **Cancel contract** — offered for **non-upfront** active contracts, and for an upfront contract not paid yet. Cancelling stops the discount from applying going forward; a paid upfront contract always runs to its end date.

*The contracts-table illustration is likewise deferred with that payment-enabled page; the descriptions above document the actual available controls rather than a fabricated image.*

## What happens at expiry

A contract stops on its end date, and so does the discount — nothing renews on its own. To keep the savings, extend before it lapses or buy a new plan. It's worth a look near the end of a term: your usage may have grown enough to justify a bigger commitment, and a better tier, than when you first signed up.
