# Account Activation

A new sign-up can't consume cloud resources until its billing profile is activated. This page covers what activation actually does, how to let ProxCloud activate accounts on its own, and how to approve or reject customers by hand.

## What an account is made of

Three objects together form a customer account:

| Object | Role |
|---|---|
| User | The person who logs in — the identity-provider subject, email, and name. |
| Project | The workspace where cloud resources (servers, volumes, networks, and so on) live. |
| Billing profile | The financial entity — it holds the balance, bills, and transactions, and links one or more users and projects. |

Activation is a state change on the **billing profile**: it moves to `ACTIVE`. At that point ProxCloud provisions the account. It bootstraps each project onto the enabled Proxmox provider(s): it creates the project's resource pool and a default security group and seeds the provider's default quota. Then it issues any configured sign-up credits. Access to the project is governed by ProxCloud's own project membership and roles; no per-user account is created on the cluster. Until then the customer can sign in but has no usable cloud project.

## Creating a user yourself

Customers normally register on their own. **Client area → Users → Create user** makes the account for them: you get a temporary password, shown once, to pass on, and the user sets their own at first login. **Delete user** also removes the login. Create user needs the bundled Keycloak with SSO enabled, which the installer sets up; with another identity provider it refuses and users appear here at their first login.

## Letting ProxCloud activate automatically

Open **System → Billing configuration** and switch to the **Activation** tab. Turn on **Auto-activation enabled** and choose the conditions a customer must satisfy for ProxCloud to activate them with no admin in the loop.

![Current Activation tab with synthetic auto-activation constraints and promotional credit example](/docs-img/billing-configuration-activation.png)

*Illustration captured from the current admin UI with synthetic local mock data; it is not evidence of live activation or provisioning.*

Each condition is a constraint set to one of three modes:

| Mode | Effect |
|---|---|
| `REQUIRED` | Must be met; activation is blocked until it is. |
| `ALTERNATIVE` | Satisfying any one alternative clears the group, as long as every `REQUIRED` constraint is also met. |
| `DISABLED` | Ignored completely. |

The constraints on offer:

| Constraint | What the customer has to do |
|---|---|
| KYC | Pass identity verification (only meaningful if a KYC integration is installed). |
| Payment method | Register any accepted payment method. |
| Payment method — card | Save a card on file (needs a card-capable gateway such as Stripe). |
| Payment method — deposit | Top up the account by at least the **Minimum deposit amount** set on the same tab. |
| Billing profile validation | Have an admin approve the profile in the validation queue (below). |

Filling in the billing address is always part of the flow — a profile with an empty billing form is never auto-activated.

## The validation queue

If **Billing profile validation** is enabled, submitted profiles arrive in **Client area → Validations** as `PENDING` entries.

![Current validation queue with a synthetic pending customer and Approve and Reject controls](/docs-img/validations-queue.png)

*Local mock illustration only; no customer decision was submitted.*

For each entry, review the customer's submitted details and any KYC evidence, then:

- **Approve** — the validation flips to `APPROVED`, the billing profile is activated (kicking off the provisioning described above), and the customer is emailed.
- **Reject** — the validation flips to `REJECTED`; the customer has to fix their details and resubmit.

## Activating by hand

With auto-activation off — or for one-off cases — you can activate a profile directly. Open it under **Client area → Billing profiles** and use the **Activate** action. Suspend and resume live in the same place.

