# Tax Rates

Tax rules tell ProxCloud what percentage to add on top of the net amount when it bills a customer. Each rule is matched against the country in the customer's billing address, so one deployment can charge different rates in different markets.

## Adding and editing taxes

Go to **System → Taxes** and use **Add tax**.

| Field | Meaning |
|---|---|
| Name | The label that appears on the invoice line, e.g. `VAT`. |
| Rate | The percentage, e.g. `19`. |
| Country | The country this rate applies to. Leave it empty to apply the rate to **all countries**. |

```
Name:    VAT
Rate:    19
Country: DE        → 19% added for customers with a German billing address
```

## How the rates combine

At invoice time ProxCloud gathers every tax rule that matches the billing profile's country — the country-specific rules plus any all-countries rules — and applies the **sum** of their rates to the taxable amount. Rates add together rather than override: if you define an all-countries base rate *and* a country rate, a customer in that country pays both. Keep your rules non-overlapping unless you actually want compound totals.

A customer whose country matches no rule is billed with no tax at all.
