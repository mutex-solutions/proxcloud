# Invoicing

Across a billing cycle ProxCloud accrues usage onto a running bill, then converts that bill into an invoice when the cycle closes. By default it produces PDF invoices itself; if your accounting lives in another system, you can hand invoice creation off to an external gateway instead.

## The built-in PDF generator

The built-in generator is the default and needs no outside service. It builds each invoice from your business details, the bill's line items, and any [tax rates](/docs/platform-admin/billing/tax) that apply. Customers download their invoices from the client portal; admins can pull them from **Client area → Customer bills**.

## The seller block

The seller details printed on every invoice come from **System → Billing configuration → Business details**:

| Field | Used for |
|---|---|
| Business name | The seller name on the document. |
| VAT id | The seller's tax identifier. |
| Country / City / Address | The seller address block. |
| Base currency | The currency of all amounts (see [Currency](/docs/platform-admin/billing/currency)). |

Fill these in before your first cycle closes — invoices that have already been issued are not regenerated.

## Routing through an external gateway

1. Open **System → Integrations** and install the invoicing integration you use, supplying its credentials.
2. Return to **System → Billing configuration → Business details** and select it in the **Invoice gateway** dropdown.
3. Save.

From the next cycle onward, invoice creation is delegated to the chosen gateway. You can switch gateways whenever you like; the change only affects invoices generated afterward. The **Mail gateway** selector beside it picks which mail integration delivers invoice and billing emails.

## The billing cycle

Bills run on a monthly cycle: usage accrues through the month and the invoice is cut when the cycle closes. The per-tick rating granularity — the minute/hour/month divisors — is set under **Billing configuration → Settings → Time unit limits**; see [Price Plans](/docs/platform-admin/billing/price-plans).
