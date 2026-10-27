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
6. Optionally tick **Pay upfront** for the upfront tiers — bear in mind upfront contracts can't be cancelled.
7. Click **Purchase**. The contract shows up in your contracts table as **ACTIVE**.

<!-- screenshot: /docs-img/savings-plans-purchase.png — Purchase form with duration selected, monthly committed amount filled in, start-month selector and the "Pay upfront" checkbox -->

## Managing your contracts

The same page lists your contracts with their plan name, duration, monthly commitment, discount rate, and end date.

- **Extend contract** — pushes the end date out by the contract's own duration (a 12-month contract gains another 12 months).
- **Cancel contract** — offered for **non-upfront** active contracts only. Cancelling stops the discount from applying going forward; upfront contracts always run to their end date.

<!-- screenshot: /docs-img/savings-contracts-table.png — Active savings contracts table showing committed/month, discount %, end date, and the Extend/Cancel actions -->

## What happens at expiry

A contract stops on its end date, and so does the discount — nothing renews on its own. To keep the savings, extend before it lapses or buy a new plan. It's worth a look near the end of a term: your usage may have grown enough to justify a bigger commitment, and a better tier, than when you first signed up.
