# How Metering and Billing Work

ProxCloud is a billing platform in front of a cloud: its job is to measure what you use, price it, and turn that into bills against your account. This page explains the model so the numbers on your invoices make sense. For the hands-on side, see [Activating Billing](/docs/getting-started/account-setup) and [Savings Plans](/docs/guides/savings-plans).

## Metering usage

The platform continuously tracks the resources in each project — how long a server ran, how much block and object storage you held, and so on — scoped to the project that owns them. This metered usage is the raw input to billing; it's gathered by the same background jobs that keep your resources in sync with the underlying cloud (see [How Provisioning Works](/docs/concepts/provisioning)).

## From usage to a bill

On a recurring cycle, a background billing job turns accumulated usage into a bill:

1. Usage over the period is priced at the operator's rates.
2. Any **savings plan** discount is applied to the usage that matches the plan. The plan's committed amount is a floor — if your matching usage falls below it in a given month, you're billed the committed minimum instead. Upfront savings payments are drawn down against these bills rather than charged again. See [Savings Plans](/docs/guides/savings-plans) for the tiers and terms.
3. The resulting bill is charged against your account.

Finished bills and the payments that settled them are kept under **Billing → Billing history**.

## Balance, credits, and how charges are covered

Your account carries a few different pots of value, all visible on **Billing → Funds**:

- **Balance** — money you've deposited. Deposits are credited here and drawn down by future bills.
- **Account credit** — credit the operator has granted you.
- **Promotional credit** — credit from a redeemed promo code (there's a **Promo code** box on the Funds page) or a sign-up/activation bonus.

A saved card, meanwhile, isn't stored value — it's a way for the platform to charge you directly when that's how your account settles.

## Why activation gates everything

Because every resource meters into a bill, the platform won't let you create billable resources until it knows who to bill and how you'll pay — that's what [account activation](/docs/getting-started/account-setup) establishes. Some operators activate accounts by manual identity or business validation instead of a deposit or card; either way, the account stays inactive, and resource creation locked, until billing is squared away.
