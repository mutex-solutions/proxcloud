# Platform Currency

ProxCloud runs on a single currency. Whatever you choose is used everywhere money shows up: price plan amounts, customer bills and invoices, account credits, deposits, and suspension thresholds.

## Choosing the base currency

Open **System → Billing configuration**, remain on the **Business details** tab, and pick the **Base currency** from the list (ISO codes with their names, such as `EUR — Euro`). The configuration won't save until a base currency is set.

![Current Base currency selector open over synthetic billing configuration](/docs-img/billing-configuration-base-currency.png)

*Current UI, synthetic local mock configuration; no currency change was saved.*

## Decide once, before launch

Set the currency during initial setup and then treat it as fixed. Changing it after customers already have bills, credits, and price plans denominated in the old currency is a bad idea — ProxCloud does no conversion, so every historical amount would silently be reread in the new unit.

Also make sure the currency is supported all the way through before you commit:

- your **payment gateway** (Stripe, for example) has to accept charges in it, and
- your **invoicing gateway** has to be able to issue documents in it.

There is no multi-currency mode and no exchange-rate handling anywhere. If you sell in several currencies, run a separate ProxCloud deployment per market.
