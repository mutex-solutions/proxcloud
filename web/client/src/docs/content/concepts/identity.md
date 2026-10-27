# How Identity Works

ProxCloud keeps no passwords of its own. Every sign-in is delegated to an OpenID Connect provider — either the **Keycloak bundled with the Helm chart** (optional, off by default) or **any external OIDC provider** you already run. The ProxCloud API behaves purely as an OAuth2 resource server: it validates the JWTs the SPAs bring back and identifies the platform user by the token's **email** claim.

## Two audiences, two realms

Customer identity and operator identity are kept deliberately apart:

| Audience | Realm (bundled Keycloak) | SPA | Default client ID | Self-registration |
|----------|--------------------------|-----|-------------------|-------------------|
| Customers | `clients` | Customer portal (`proxcloud-web`) | `proxcloud-ui` | Enabled |
| Operators/admins | `master` | Admin console (`proxcloud-admin`, under `/proxcloud_admin`) | `proxcloud-admin`, plus `proxcloud-admin-api` for API access | Disabled — admins are provisioned by hand |

Both SPAs are public OIDC clients running the authorization-code flow with PKCE. Authenticating against the admin realm marks a user as an administrator; a customer holding a `clients`-realm token can never reach the admin console.

## The bundled Keycloak (optional)

Keycloak is off by default. Set `keycloakx.enabled: true` and the chart runs Keycloak — the official image, via [codecentric/keycloakx](https://github.com/codecentric/helm-charts/tree/master/charts/keycloakx) — pointed at a PostgreSQL you provide (the bundled `postgresql`/`cnpg`, or a managed one; set `keycloakx.database.*`). Enable `keycloakConfigCli.enabled: true` (or supply a realm export through `realmImport.json`) to create both realms, the three clients and their redirect URIs; set the bootstrap admin in `keycloakx.extraEnv`, and configure SMTP on the realm for verification and password-reset mail. Expose Keycloak yourself via `keycloakx.ingress.*`.

Because Keycloak fronts identity, its features come along at no extra cost: email verification, password recovery, TOTP two-factor auth, and identity brokering — social logins like Google or GitHub, or federation with LDAP, Active Directory, or SAML. You configure these per realm in the Keycloak admin console.

Registration itself is realm-side. The customer SPA simply redirects to the authorize endpoint, and Keycloak shows the "Register" link because the `clients` realm has user registration turned on, with **email as the username**. The first time a user signs in, ProxCloud creates the platform user from the token — get-or-create by email — and runs any sign-up bonus or activation logic.

## Bringing your own OIDC provider

Set `keycloakx.enabled: false` and aim ProxCloud at your own provider — an existing Keycloak, Okta, Auth0, or anything spec-compliant:

```yaml
keycloakx:
  enabled: false
auth:
  main:
    issuer: "https://auth.example.com/realms/clients"        # customer issuer
  admin:
    issuer: "https://admin-auth.example.com/realms/master"   # admin issuer
  adminApi:
    issuer: "https://admin-auth.example.com/realms/master"   # admin-API issuer
```

`auth.main.issuer` and `auth.admin.issuer` are independent: your customer auth domain and your admin auth domain can differ, and can even live on entirely different providers.

On an external IdP the chart provisions **nothing** — you set it up yourself, per realm/tenant:

- A **public** OIDC client (authorization code + PKCE) whose client ID matches the configured one, with `redirectUris` covering the app URL plus `/*` and `webOrigins` set to the app origin. The SPAs redirect back to `<app origin>/` exactly, so that URL must be permitted.
- Tokens that carry the **email** claim.
- On the end-user side only: self-registration enabled **with email as the username**. ProxCloud keys both users and billing profiles by email — username-based registration breaks user creation and billing.
- SMTP on the realm, so verification and reset mails actually send.

The client IDs stay configurable through `auth.main.clientId`, `auth.admin.clientId`, and `auth.adminApi.clientId` (defaults `proxcloud-ui`, `proxcloud-admin`, `proxcloud-admin-api`).

## One identity across OpenStack too

The same Keycloak can authenticate users against OpenStack Keystone and Horizon as well, giving customers a single login across ProxCloud and native OpenStack tooling. That federation is an operator setup task — see [Single Sign-On](/docs/self-hosting/sso).
