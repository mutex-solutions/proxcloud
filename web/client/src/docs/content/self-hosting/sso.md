# Single Sign-On with OpenStack

By default ProxCloud and OpenStack authenticate separately: ProxCloud against Keycloak (or your external OIDC provider), Keystone against its own local user database. This page is about joining them, so one account works everywhere — the ProxCloud portal, Horizon, and the OpenStack CLI. The identity model itself is covered in [How Identity Works](/docs/concepts/identity).

## The two issuer slots

ProxCloud always speaks OIDC, and the chart gives you two issuer "slots":

```yaml
auth:
  main:
    issuer: "https://auth.example.com/realms/clients"        # customers
  admin:
    issuer: "https://admin-auth.example.com/realms/master"   # operators
  adminApi:
    issuer: "https://admin-auth.example.com/realms/master"   # admin API
```

- **`auth.main.issuer`** authenticates customers (the portal, `proxcloud-ui` client).
- **`auth.admin.issuer`** authenticates operators (the admin console, `proxcloud-admin` client), and **`auth.adminApi.issuer`** guards the machine-to-machine admin API (`proxcloud-admin-api` client).

They're independent by design: customer identity and staff identity usually live in different realms — often different domains — and only the customer realm should ever be federated toward OpenStack. With the bundled Keycloak these are the `clients` and `master` realms, and the split is wired up automatically.

## Federating Keystone with Keycloak

To let the same accounts sign in to native OpenStack tooling, make Keystone an OIDC relying party of the **customer** realm. In outline:

1. **Keystone as an OIDC client.** Register a confidential client for Keystone in the `clients` realm. Keystone's Apache frontend runs `mod_auth_openidc` aimed at the realm's discovery URL (`…/realms/clients/.well-known/openid-configuration`) with that client ID and secret.
2. **Identity provider object.** In Keystone, create an identity provider that represents Keycloak, a federation **protocol** named `openid` bound to it, and a **mapping**.
3. **Mapping rules.** The mapping turns OIDC claims into Keystone users — commonly mapping the email/subject claim to an ephemeral federated user in a chosen domain and group, from which project role assignments follow.
4. **Horizon WebSSO.** Enable WebSSO in Horizon (`WEBSSO_ENABLED = True` with an `openid` choice) so the dashboard's login page offers "Authenticate using Keycloak" and drives the redirect through Keystone.

With Kolla Ansible, most of this collapses into enabling Keystone federation in `globals.yml` (`keystone_identity_providers` / `keystone_identity_mappings` pointing at your realm) and letting Kolla template the Apache / `mod_auth_openidc` configuration.

The payoff: one Keycloak account works in the ProxCloud portal (direct OIDC) and in Horizon and the CLI (through Keystone federation). ProxCloud still manages each customer's OpenStack projects with its own service credentials — federation only adds human sign-in to native tooling and is never required for ProxCloud to run.

## Layering extras onto the bundled Keycloak

Since Keycloak sits in front of everything, SSO extras are realm configuration, not ProxCloud configuration:

- **Social logins** (Google, GitHub, …) — add identity brokers to the `clients` realm.
- **Corporate directories** — federate the realm with LDAP / Active Directory or a SAML2 IdP; handy on the `master` realm so staff use corporate accounts for the admin console.
- **2FA / passkeys** — enable TOTP or WebAuthn policies per realm.

If you take over realm settings by hand after installation, note that when `keycloakConfigCli.enabled: true` the chart runs a config-CLI job on install/upgrade that re-applies the provisioned realm state — set `keycloakConfigCli.enabled: false` once you own that configuration manually.

## Using an external IdP instead

All of the above works just as well with an external Keycloak or another OIDC provider: set `keycloakx.enabled: false`, fill in `auth.main.issuer` / `auth.admin.issuer`, create the clients yourself (see [How Identity Works](/docs/concepts/identity) for the exact client requirements), and federate Keystone against that provider's customer realm the same way.
