# Tenant authorization: order reads, role assignment and private sandbox traffic

These corrections enforce existing tenant boundaries. They do not enable mail,
snapshot lifecycle, registry deletion, worker rotation or any other held feature.
They introduce no schema, dependency, credential or external service.

## C1 — Actors and protected data

| Actor | Required authority | Protected result |
| --- | --- | --- |
| Client reading an order | `billing_profile:read` on the organization owning its stored billing profile | Typed order totals, line items and metadata |
| Organization member adding a person | `organization:manage_members`; also `organization:manage_roles` for any role other than `MEMBER` | The new member's effective role |
| Caller reaching a private native sandbox | That sandbox's traffic token on **every** application port, including `49983` | The owner's private application response |
| Caller reaching envd on an envd-enabled pod | envd's own access token, checked by envd | envd commands/files; control-plane routes remain blocked by the proxy |

Knowing an order ID, a custom role name or a numeric port is not authorization.

## C2 — Containers and trust boundaries

```text
Client bearer -> API authentication -> order handler
  -> order's stored billingProfileId -> billing authorizer
  -> owning organization -> current org RBAC -> typed response or opaque 404

Organization member -> API authentication -> membership-management permission
  -> role-management permission unless MEMBER -> known org role
  -> verified email -> membership insert -> existing project propagation/audit

Internet -> sandbox-proxy -> ready pod lookup (IP, traffic token, runtime mode)
  -> token gate -> native application OR envd's own authentication
```

All components already exist. Authentication, billing and role storage continue
using the existing API and primary PostgreSQL repositories. No new network path or
runtime privilege is added.

## C3 — Component responsibilities

- `order.Handler` resolves the actual stored order, not a caller-provided profile.
  Its required `ProfileReader` callback is wired to
  `org.BillingHandler.CanReadBillingProfile` in `cmd/api/main.go`. Missing callback,
  missing profile/organization or denied permission produces `404 Order not found`,
  identical to a missing order. Repository failures remain generic errors, not
  successful empty responses. The response is a typed `order.Order`, not a raw
  storage document.
- The billing authorizer loads the profile's stored organization and applies the
  existing `billing_profile:read` rule. A role in a different organization grants
  nothing. Custom role and membership changes are read again on later requests.
- `org.Handler.addMember` preserves ordinary `MEMBER` invitations for a caller
  with membership-management authority. Every custom-role assignment also requires
  role-management authority, even if its current permissions look harmless: custom
  permissions can change later. Both add-member and role-update reject unknown
  role names and malformed JSON before changing membership. Existing direct
  operator/admin API policy is unchanged.
- `sandbox.PodRunsEnvd` classifies the **actual main container command** returned
  by the Kubernetes API, not a caller header or the currently configured provider
  mode. `sandbox-init` with legacy absent/empty args or exactly `--serve` identifies
  envd; `-native`, utility modes and malformed shapes do not. This supports existing
  envd pods without requiring a new annotation or recreating workloads.
- `sandboxproxy.K8sLookup` carries that mode with its existing IP/token result.
  Only an envd pod receives the port-49983 traffic-token exemption. Native private
  services on that number require the traffic token like other application ports.

## C4 — Decisions and regression contracts

### Order read

1. Resolve the authenticated user and load the typed order.
2. Refuse absent orders, empty ownership links or an unwired authorizer.
3. Resolve the stored profile and its live owning organization.
4. Require current `billing_profile:read`; otherwise answer the same opaque 404.
5. Return only the typed DTO after success.

Native PostgreSQL regression coverage includes OWNER/MEMBER/custom-reader success,
foreign-organization and restricted-custom-role denial, broken ownership links,
missing authorizer, typed field preservation, exclusion of unknown storage fields,
and revocation observed between requests.

### Membership assignment

1. Require membership in the target org and `organization:manage_members`.
2. Decode the request; require `organization:manage_roles` unless role is MEMBER.
3. Resolve any custom role in that same organization; reject unknown names.
4. Resolve an IdP-verified email, then perform existing membership and audit work.

Native PostgreSQL tests check real handler/repository/policy results, including
ordinary invitation success, authorized custom-role success, denied POWER/wildcard
role assignment, no row after denial, and invalid role/body refusal. This deliberately
uses the same role-management authority as changing an existing member's role;
it does not claim atomicity with a separate concurrent role-definition editor.

### Private traffic

The proxy still blocks all canonicalized envd control paths on port 49983 before
lookup, for both runtime modes. Public native traffic remains public. Envd's token
is not a substitute for a native application's traffic token. Envd keeps validating
its own token: this patch does not move that secret into the proxy.

Tests build production pod templates, round-trip them through JSON, invoke the
actual pod lookup and proxy, and use owned loopback listeners. Denied native
requests must not even open an upstream connection. Positive native, public,
ordinary-port and envd-own-auth controls prevent an accidental deny-all fix.

## Acceptance limits

All executable tests run in Parent-controlled, no-external-network sandboxes with
read-only source/dependencies and bounded private scratch. PostgreSQL tests use an
owned private PostgreSQL 17 in the same nonroot, network-none container; external
overlays adapt only existing Testcontainers fixture bootstrap, not assertions or
production behavior. These are source/regression claims, not live token issuance,
real Kubernetes admission, SMTP delivery, whole-cloud isolation or release approval.
The two-second pod cache and its independent address-reuse question are unchanged.
