# Automatic Suspension

Automatic suspension is your defense against customers who keep consuming resources without paying. ProxCloud watches each billing profile, fires off escalating warnings as the account slides, and finally suspends it — pausing the account's cloud resources — once the configured threshold is crossed.

## Turning it on and picking a trigger

Open **System → Billing configuration**, go to the **Settings** tab, and enable **Automatic suspension**. Exactly one trigger type is active at a time:

| Type | Watches | Thresholds are expressed in |
|---|---|---|
| `BALANCE` | The account balance. | The **Balance** fields (typically zero or negative amounts). |
| `DUE_DATE` | Unpaid invoices. | The **Days** fields (days past the invoice due date). |

## Warnings and the cut-off

Two things are set in the same section:

- **Notification limits** — a list of checkpoints. When the balance drops to a listed value (or an invoice ages past a listed day count), the customer gets a warning email. Add as many as you like with **Add limit**.
- **Suspend at** — the final threshold (**Suspend at — balance** or **Suspend at — days**, depending on the trigger type) at which the profile is actually suspended.

A sample balance policy:

![Current Settings tab illustrating a synthetic balance-based suspension policy](/docs-img/suspension-balance.png)

*Local mock illustration of balance fields; no suspension policy was saved.*


```
Type: BALANCE
  Notify at:     0        "your balance is exhausted"
  Notify at:   -10        "top up now to avoid suspension"
  Suspend at:  -20        account suspended
```

A sample due-date policy:

![Current Settings tab illustrating synthetic overdue-day thresholds with balance fields cleared](/docs-img/suspension-due-date.png)

*Local mock illustration of overdue-day fields; no policy or customer state was changed.*


```
Type: DUE_DATE
  Notify at:   3, 5, 7 days after the invoice due date
  Suspend at:  10 days     account suspended
```

## What a suspension does

Suspending a billing profile suspends the projects it pays for: those attached to it directly, and those in its organization with no profile of their own. Each project is set to `DISABLED` and its running VMs are suspended in Proxmox (`qm suspend`). The data itself is kept — suspension is reversible, not destructive. Suspended profiles show their status under **Client area → Billing profiles**.

While a project is `DISABLED` its members can still read everything, stop VMs and delete resources. Creating anything, starting or rebooting a VM, opening its console, resizing, uploading objects and creating or rotating S3 keys all answer 409 with an explanation. S3 keys already issued keep working against the object store itself.

## Bringing an account back

Once the customer settles up — tops up the balance or pays the overdue invoice — resume the profile from its page under **Client area → Billing profiles**. Its projects are set back to `ENABLED` and their suspended VMs are resumed. A project you disabled yourself stays disabled, and disabling one a suspension already holds keeps it down past the resume. Chronic non-payers can be terminated by hand from that same page once your own grace policy has run its course.
