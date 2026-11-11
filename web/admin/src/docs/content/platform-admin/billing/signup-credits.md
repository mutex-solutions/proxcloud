# Sign-up Credits

Provisioning promotional credits are a welcome bonus: a chunk of platform credit granted automatically to every new customer at activation, letting them kick the tires on your cloud before spending real money.

## Setting up the grant

Open **System → Billing configuration**, switch to the **Activation** tab, and find the **Provisioning promotional credits** section. Use **Add credit** to define one or more grants:

![Current Activation tab including a synthetic provisioning promotional credit row](/docs-img/billing-configuration-activation.png)

*The shared Activation illustration uses local mock data; it does not grant real credit.*


| Field | Meaning |
|---|---|
| Amount | The credit value in the platform currency, e.g. `100`. |
| Days valid | The credit's lifetime; after this many days any unused remainder expires, e.g. `30`. |

```
Amount:      100
Days valid:  30      → every new customer starts with 100 credit, valid one month
```

Save the configuration; the change takes effect for customers activated afterward. Delete every row if you'd rather not offer a sign-up bonus — with an empty list, no credit is granted at provisioning.

## When credit is granted, and when it's spent

The grant fires when the billing profile is **activated** and its project provisioned (see [Account Activation](/docs/platform-admin/account-activation)) — not at form sign-up — so registrations that never verify never get credit.

Granted credit lands as account credits on the billing profile and is consumed by usage charges before the paid balance is ever touched. Anything still unspent when the validity window closes is forfeited. Admins can inspect a customer's credits on the billing profile page under **Client area → Billing profiles**.

Note that these provisioning credits are a different thing from **promotion codes** (System → Promotion codes), which customers redeem themselves by entering a code.
