# Single Sign-On

ProxCloud delegates every sign-in to OpenID Connect, so single sign-on is set up on the identity provider: Keycloak realms (bundled or your own), or another OIDC provider. This page covers how the issuers are split between customers and operators, and how to layer social logins, corporate directories and two-factor policies on top. The identity model itself is covered in [How Identity Works](/docs/concepts/identity).

ProxCloud manages each project's Proxmox VE resources with its own API token, so customers never need Proxmox accounts and nothing has to be federated toward the Proxmox clusters.

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

They're independent by design: customer identity and staff identity usually live in different realms — often different domains. With the bundled Keycloak these are the `clients` and `master` realms, and the split is wired up automatically.

## Layering extras onto the bundled Keycloak

Since Keycloak sits in front of everything, SSO extras are realm configuration, not ProxCloud configuration:

- **Social logins** (Google, GitHub, …) — add identity brokers to the `clients` realm.
- **Corporate directories** — federate the realm with LDAP / Active Directory or a SAML2 IdP; handy on the `master` realm so staff use corporate accounts for the admin console.
- **2FA / passkeys** — enable TOTP or WebAuthn policies per realm.

If you take over realm settings by hand after installation, note that when `keycloakConfigCli.enabled: true` the chart runs a config-CLI job on install/upgrade that re-applies the provisioned realm state — set `keycloakConfigCli.enabled: false` once you own that configuration manually.

## Using an external IdP instead

All of the above works just as well with an external Keycloak or another OIDC provider: set `keycloakx.enabled: false`, fill in `auth.main.issuer` / `auth.admin.issuer`, and create the clients yourself (see [How Identity Works](/docs/concepts/identity) for the exact client requirements). Social logins, directories and 2FA are then configured in that provider.
