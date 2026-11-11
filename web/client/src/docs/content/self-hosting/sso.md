# Single Sign-On

ProxCloud delegates every sign-in to OpenID Connect and runs one customer realm in Keycloak. There are two kinds of SSO on top of that:

- **Per-organization SSO.** An organization connects its own identity provider (Microsoft Entra ID, Okta, Google Workspace, any OIDC or SAML provider) from the console, without involving you. This page covers enabling it and what it guarantees.
- **Platform-wide extras.** Social logins or a directory for everybody, added by you as realm configuration.

ProxCloud manages each project's Proxmox VE resources with its own API token, so customers never need Proxmox accounts and nothing has to be federated toward the Proxmox clusters. The identity model itself is covered in [How Identity Works](/docs/concepts/identity).

## The two issuer slots

The chart gives you two issuer slots:

```yaml
auth:
  main:
    issuer: "https://id.example.com/realms/proxcloud"        # customers
  admin:
    issuer: "https://id.example.com/realms/proxcloud-admin"  # operators
  adminApi:
    issuer: "https://id.example.com/realms/proxcloud-admin"  # admin API
```

Keep them in different realms. A customer's identity provider then has no path to an operator account. The installer does this by default.

## Enabling per-organization SSO

It needs Keycloak (bundled, or your own with a service account) as `auth.main`. An installation with another OIDC provider leaves the feature off.

```yaml
auth:
  sso:
    enabled: true
    clientSecret: "<random>"            # or the key sso-client-secret in api.existingSecret
    reservedDomains: ["example.com"]    # your operators' email domain
keycloakConfigCli:
  enabled: true
```

The installer turns this on and generates the secret.

`keycloakConfigCli` adds these to the customer realm, from `files/keycloak-sso.json` in the chart. Apply the same file yourself if you run Keycloak separately:

- the client scope `proxcloud-sso`, which puts `idp` and `idp_groups` in tokens;
- the confidential client `proxcloud-idp-admin` with a service account, limited to `manage-identity-providers`, `view-identity-providers`, `query-users`, `view-users` and `manage-users`, plus the `kc-admin-api` scope Keycloak 26.8 requires for Admin API tokens;
- the read-only user attribute `idp_groups`.

Pin Keycloak to 26.8.0 or newer, which fixes CVE-2026-14781, CVE-2026-12388 and CVE-2026-19608 in identity brokering.

The domains of `api.selfUrls` are always reserved. `PROXCLOUD_SSO_ALLOW_INTERNAL_IDP` (`auth.sso.allowInternalIdp`) lets a provider live on `http` and on private addresses; it is for development only.

## What an organization does

Under **Organization, Single sign-on**, someone with `organization:manage_roles`:

1. Registers ProxCloud at their provider using the redirect URI (OIDC) or service provider metadata URL (SAML) shown on the page.
2. Adds their email domain and publishes the TXT record `_proxcloud-challenge.<domain>`. The domain counts once ProxCloud has seen the record.
3. Enters the discovery or metadata URL, the client ID and secret, and how groups map to roles.
4. Optionally turns on enforcement, after naming a break-glass owner.

Their people then enter their work email on the sign-in page and land at their provider.

### Microsoft Entra ID

Create an app registration with a web redirect URI. Create a client secret. The discovery URL is `https://login.microsoftonline.com/<tenant-id>/v2.0/.well-known/openid-configuration`. Entra does not send `email_verified`, so keep **Trust email from the provider** on. To map groups, add a groups claim under Token configuration; Entra sends group object IDs unless you configure otherwise, and those IDs are what you enter in the mapping.

### Okta

Create an OIDC web application with the redirect URI. The discovery URL is `https://<org>.okta.com/.well-known/openid-configuration`. To map groups, add a `groups` claim to the ID token and add `groups` to **Scopes**: `openid email profile groups`.

### Google Workspace

Create an OAuth client of type Web application with the redirect URI. The discovery URL is `https://accounts.google.com/.well-known/openid-configuration`. Google sends no groups, so everyone gets the default role. Google accepts any Google account at this client, and the verified-domain rule is what keeps others out: an address outside your domains is refused.

## What ProxCloud guarantees

Enforced in the API on every request, not only at login:

- **A domain has one owner.** A domain counts only after the DNS TXT check, and a unique index keeps it to one organization. A pending claim blocks nobody, so a domain cannot be squatted. Reserved domains can never be claimed.
- **A provider speaks only for its own domains.** A token from an organization's provider with an email outside that organization's verified domains is refused and creates no ProxCloud user, so nobody can invite the fake person by email.
- **Accounts do not change hands.** A person stays bound to the provider that first provisioned them. Keycloak's first-broker-login flow never links an existing account without a password or an email round trip, and a provider linked to an account anyway cannot use it. A local account can be claimed only by the provider that owns the domain of its existing address.
- **SSO never creates an admin.** It cannot grant OWNER, ADMIN or a role that can manage members or roles. This is checked when the mapping is saved and again when it is used, and an existing membership is never changed.
- **Provider metadata stays outside your network, at the time it is saved.** ProxCloud fetches discovery documents and metadata itself, refusing private addresses, and rejects a document that points at them. Keycloak then calls those URLs at every login, out of ProxCloud's reach, and a tenant controls their DNS: a name that resolved to a public address when saved can resolve to an internal one later. Only your network can close that, so **restrict Keycloak's egress**, see below.
- **Secrets stay encrypted.** The client secret is write-only and stored with `textcrypt`. SSO refuses to save one if no encryption key is configured.

Every configuration change, automatic provisioning, refused login and break-glass use goes to the audit log.

An email a provider asserts for a foreign domain still creates a Keycloak account during the broker login, because Keycloak runs before ProxCloud sees the token. ProxCloud removes such accounts every 5 minutes when they hold nothing else. Until then the address's real owner sees "account already exists".

### Required: Keycloak egress policy

With per-organization SSO on, tenants choose URLs that Keycloak requests server-side (DNS rebinding makes the check at save time insufficient). Keycloak must not be able to reach anything internal:

- Allow egress to DNS and to the public internet on 443.
- Deny the pod network, the service network, the node and management networks, your Proxmox, TrueNAS and database addresses, and link-local (`169.254.0.0/16`, cloud metadata included). Keycloak's own database stays allowed.
- On Kubernetes this is a `NetworkPolicy` with an `ipBlock` of `0.0.0.0/0` and an `except` list on the Keycloak pods, which needs a CNI that enforces it. The chart does not ship one, since the ranges are yours. With another IdP the same applies to whatever brokers the logins.

### Enforcement

With enforcement on, members of the organization and everyone with a verified-domain address must arrive through that provider. Passwords and API tokens stop working for them, and existing tokens of members are deleted. Members whose email is outside the verified domains are locked out too, unless they are break-glass owners.

Up to two owners can be named break-glass. They keep their password, the bypass ends if they stop being an owner, and each use is audited. Operators live in their own realm and are never affected.

### Disabling

An operator can stop any provider under **System, Organization SSO**. Its sessions are refused within a minute, enforcement stops applying, and the organization cannot turn it back on. The same page releases a domain after a dispute.

### Limits

- One provider per organization.
- Roles do not follow group changes after the first sign-in, and a person removed at the provider keeps their session until it expires. Set the realm's SSO Session Max to what you can accept.
- Prefer disabling to deleting a provider. Deletion permanently retires its platform-issued alias before removing Keycloak links, so retained brokered tokens are refused even if it was the last configured provider. Returning people use a new provider alias and may need the email-link round trip. Cleanup failures stay retired; retry DELETE using an authorized non-retired local/break-glass session. All API replicas must run retirement-aware code. Existing cached rules on other replicas can remain stale during a primary database outage; this is not a hard one-minute outage revocation SLA. Previously deleted aliases cannot be reconstructed automatically.
- A domain stays verified after the TXT record is removed.

## Layering extras onto the bundled Keycloak

Since Keycloak sits in front of everything, these are realm configuration, not ProxCloud configuration:

- **Social logins** (Google, GitHub, …): add identity brokers to the customer realm. They appear for every customer.
- **Corporate directories for your own staff**: federate the operator realm with LDAP, Active Directory or a SAML2 provider.
- **2FA and passkeys**: enable TOTP or WebAuthn policies per realm.

`keycloakConfigCli` re-applies its realm state on every install and upgrade. It never deletes identity providers (`IMPORT_MANAGED_IDENTITY_PROVIDER=no-delete`), so organization providers survive upgrades. Set `keycloakConfigCli.enabled: false` once you own the rest of the realm configuration by hand.

## Using an external IdP instead

Set `keycloakx.enabled: false`, fill in `auth.main.issuer` and `auth.admin.issuer`, and create the clients yourself (see [How Identity Works](/docs/concepts/identity) for the exact client requirements). With another OIDC provider in `auth.main`, leave `auth.sso.enabled` off: per-organization SSO needs the Keycloak Admin API.
