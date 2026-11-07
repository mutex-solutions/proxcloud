# C4 model change log

Every architecture change lands here in the same PR that changes the code and the
diagrams: what changed, why, and which diagrams were updated. Newest first. Format
and rules: [README.md](README.md#maintenance-rule).

## 2026-11-07: Installer re-runs keep operator settings, split mode passes preflight

**What changed**

- Stage 60 on an existing Proxmox provider keeps its storage profiles, default profile and confirmed classes, and only
  adds what was newly detected. A value in the site's group_vars still wins.
- Stage 60 registers the chart versions of the checkout instead of pins in `versions.yml`.
- Preflight accepts `mode: split` and checks its settings. `object_store.mode` must be `vm` or `truenas-app` there.
- Stage 10 requires the CAPI and CSI token secrets in split mode, and takes them from the vault when the tokens exist.

**Why**

- `make upgrade` runs stage 60, and each run replaced those fields with fresh detection. A class confirmed in the admin
  console was lost, and Ceph added to a Standard cluster moved the default profile, so new VMs changed storage with no
  one asking.
- Stage 50 pushes the charts of the checkout. The pin had fallen behind, so a clean split install would register a
  chart version that is not in its registry.
- Preflight still refused split mode with a message from before it existed, so the documented command could not start.

**Diagrams updated:** none; no container or relationship changed.

## 2026-11-07: Server snapshots billed (MET-G9)

**What changed**

- The full sync and portal actions stamp `server.snapshots {items, diskGb}` on the SERVER row; the minute sampler
  carries it. `ServerProvider` turns each snapshot into a `server_snapshot` billing resource with the snapshot's own
  start time, priced by a new seed rule. `stampResourceValues` keeps a start time a provider set.

**Why**

- Snapshots hold space on the operator's storage and were free. Proxmox reports no per-snapshot usage on any backend
  (a volume's `used` excludes its snapshots), so the metered size is the provisioned disks: an upper-bound proxy with a
  placeholder rate, until a node-side read justifies real usage. One line per snapshot because a bill item's first
  watermark is the resource's start: a single line per server would back-charge a new snapshot to the server's creation.

**Diagrams updated:** none (the metering flow is unchanged, one more billing resource on the SERVER row).
## 2026-11-07: Split mode keeps the portal's SDN rights

**What changed**

- `mode: split` also grants `ProxCloudPlatform` on `/sdn/zones` to the portal token. Test connection reads `/sdn/zones`
  as well as `/sdn`.

**Why**

- An ACL on a path replaces what its parent passes down. The mint grant alone took `SDN.*` away from the portal, so on
  a split site no tenant network could be created. The lab showed it as soon as the grants were applied. No diagram
  shows ACLs.

**Diagrams updated:** none.

## 2026-11-07: Object store connection checked at registration, public buckets always closable

**What changed**

- New `POST /admin/service/object-store/test`: the API checks a ceph-s3 endpoint and admin key without storing them.
  Installer preflight requires the `ceph-rgw` values, stage 60 calls the test before it writes, updates the row's
  endpoint and key on a re-run, and registers `object_store.region` as the provider's region.
- A bucket's website state now also reads its public-read statement. Disable website removes that statement before it
  touches the website config, and making a bucket private removes it too.

**Why**

- A wrong endpoint or key stored as a PUBLIC provider made every new project fail its bootstrap, after a green install,
  and a re-run skipped the row. Checking from the API tests the network path the running platform will use. The region
  shown to tenants was the Proxmox region name, which RGW rejects as a location constraint.
- The statement publishes objects on the S3 endpoint whether or not the gateway has its website API. A gateway answering
  405, or a provider with no website endpoint, left tenants with a public bucket and no control that closed it.

**Diagrams updated:** none; no container or relationship changed.
## 2026-11-07: Money paths: bill writers under the credit lock, unknown outcomes stay PENDING, unpaid upfront contracts held

**What changed**

- A recorded manual payment and a card charge settling on a bill now hold the profile's credit lock, like paying with
  credits and issuing a bill. A charge the bill already holds changes nothing, and one succeeding on a bill paid some
  other way becomes credit.
- The transaction scanner closes a transaction only when the gateway answers it has no such intent. Any other error
  leaves it `PENDING` with the reason, re-checked until it leaves the 24 hour window.
- An upfront savings contract with no deposit and no operator on record never discounts. Such contracts that are `ACTIVE`
  move to `PENDING_PAYMENT` at API start, and a new operator action records a payment taken outside the portal.

**Why**

- Each bill writer replaces the whole bill it read, so concurrent payments overwrote each other: a race test of 16
  payments of 5 kept 50 of the 80 on the bill. A scanner marking a deposit `FAILED` on a network error needed a manual
  repair if the money had moved, since `FAILED` is terminal, and it could also overwrite a settle that won meanwhile.
  Upfront contracts bought before payments were enforced discounted every bill for money nobody paid.

**Diagrams updated:** none, no container or component changed.

## 2026-11-07: Admin hmac key secrets encrypted at rest, old NFS exports squash root

**What changed**

- `hmac_keys.secretKey` is stored encrypted with the data-at-rest key. Every writer seals it, the SigV4 and MCP lookup
  opens it, and rows in plain text are converted at API start. Creating a key still returns its secret once.
- The share reconcile treats an NFS export with `maproot` as drifted: it opens the top directory and clears `maproot`.

**Why**

- A database read handed over every Admin API credential in the clear. SigV4 has to get the secret back, so a hash like
  the one client tokens use cannot work. A key changed without re-sealing fails closed: the stored text does not start
  working as the secret.
- Root squash only covered new shares, so the oldest ones, with the most data, kept giving a client's root full access
  on the NAS. The top directory opens because otherwise a squashed client could not write to a root-owned dataset at all.
  It is not recursive, so files root wrote before stay owned by root, which `docs/cloud-integration.md` tells operators.

**Diagrams updated:** none, no container or component changed.

## 2026-11-07: Shared block on a TrueNAS LUN checked (P8-4, part)

**What changed**

- Shared LVM on a TrueNAS iSCSI LUN ran as a storage profile: documented as a site setup, marked Implemented. A failed
  volume action now refreshes the project's volume rows.

**Why**

- It needed no code, only proof: detection, profile checks, cross-node clone, live migration and cross-node attach all
  held. A failed attach had left a stale row naming a disk that had moved to a holder, so the retry failed until the
  next sync. The storage plugin, multipath, LINSTOR and democratic-csi remain Planned.

**Diagrams updated:** `c2-containers.md` (storage table).

## 2026-11-07: Tenant DNS zones on PowerDNS (P8-3)

**What changed**

- New provider kind `powerdns` and package `internal/cloud/powerdns`: tenants create zones and edit records in the
  customer console (`DNS_ZONE` rows, `LIST_RECORDS`, `SET_RECORD`, `DELETE_RECORD`). The zone's `account` field names
  the owning project. The sync reconciles rows, and project teardown deletes zones.

**Why**

- DNS was a gap against the earlier cloud backend. A separate provider keeps the DNS server optional and out of the PVE
  path, the same shape as TrueNAS shares. The account field is the one ownership proof, since zone names are global on
  the server.
- The PVE SDN DNS plugin stays Planned: wiring it means changing the shared EVPN zone, and its value depends on IPAM
  records this platform does not create today.

**Diagrams updated:** `c1-context.md` (new external system).

## 2026-11-07: Seeds stay with their project, a retype keeps its row

**What changed**

- A seed is looked up by service, project and vmid. The sync drops seeds whose VM is gone or whose vmid now belongs to
  another project.
- A volume retype repoints the row at the new volid (`cloud.Repo.ChangeExternalID`) instead of archiving it.
- `POST /admin-api/v1/users` without a `sub` makes the Keycloak login, or answers 400 with no service account.

**Why**

- PVE reuses vmids, and a seed holds user-data, so a stale seed could reach another tenant's VM on its next reseed.
- A bill is keyed on the row id, and the same disk showed as deleted next to a new one after a change of type.
- A generated `sub` matched no login, so the created user could never sign in.
- Keycloak calls tenant-chosen SSO URLs out of the API's reach, and DNS can change after the check at save time, so
  the egress policy is now a documented install requirement rather than something the code claims to cover.

**Diagrams updated**: none: no container or component changes.

## 2026-11-07: Money paths: upfront contracts are paid for, credit has one writer per profile

**What changed**

- Upfront savings contracts start `PENDING_PAYMENT` with a server-computed `upfrontAmount`. Only a deposit bound to the
  contract at `AddFunds` activates it, and only `ACTIVE` contracts discount a bill.
- `Repo.WithProfileLock`: pay-from-credit, bill issue and refund read and write credit in one transaction under a
  per-profile advisory lock.
- A deposit's credit has the transaction's id, and the scanner re-drives a deposit stuck `PROCESSING`.
- Bank transfers leave `PENDING` once, through a conditional update.
- A refund refuses a credit partly spent or a contract still running.

**Why**

- A contract was `ACTIVE` at create with no payment, and upfront means usage up to the commitment is free, so a tenant
  got that usage for nothing. Any deposit naming any contract id activated it, whatever the amount or owner.
- Credit was read, checked and replaced with no lock, so two concurrent payers both spent the same balance.
- An advisory lock rather than a row lock: the balance is many rows, and a lock on one profile row would tie the
  money paths to that row existing.
- The settle released its claim on any error after minting, so a retry minted again. A primary key is the one guard that
  also covers a crash between the two writes.

**Diagrams updated**: none: no container or component changes.

## 2026-11-07: Kubernetes RWX on the Standard tier (P7-9)

**What changed**

- With `config.cluster.rwxGiB` set, a new tenant cluster gets one NAS dataset with a `refquota`, the `shared-file`
  StorageClass and csi-driver-nfs, installed by CAAPH. The dataset is an ordinary share of the project named
  `k8s-<clusterId>`, and goes with the cluster. Chart 0.1.3.
- A cluster gets a marker secret, and the orphan sweep keys on it instead of the cloud secret.

**Why**

- proxmox-csi and zfspool are RWO only, and the storage plan puts RWX on the NAS for Standard sites. Using the share
  product means the dataset bills, counts against the share quota and follows the project's addresses with no second
  code path. Full-tier CephFS remains to be built.
- CAPMOX takes ownership of the cloud secret, so it is garbage-collected with the `ProxmoxCluster`. The sweep never saw
  a finished cascade: addresses and minted tokens of a deleted cluster were never released. Found deleting a real one.

**Diagrams updated:** `c2-containers.md` (storage table).

## 2026-11-07: Full tier run on a real Ceph cluster

**What changed**

- A detached volume on shared storage attaches to a server on any node (`carryToNode`): a new holder VM takes the disk,
  migrates offline, and the disk is reassigned on the target's node. `LIST_ATTACH_TARGETS` lists every server of the
  project for such a volume. Local disks keep the same-node rule.
- The stale cloud-init guard on migrate skips shared storage.
- RGW static websites are offered only when `s3WebsiteEndpoint` is set. A gateway without `rgw_enable_static_website`
  (`405`) reports "does not offer static website hosting" instead of an internal error.

**Why**

- The Full tier had only been unit-tested. The plan sells Ceph RBD as attachable on any node, and PVE's reassign refuses
  two nodes even on shared storage, so the code refused it. The refusal was right for local disks only.
- The guard read a shared volume, which every node lists, as a leftover from a failover: no VM with a cloud-init disk
  could live-migrate on RBD or NFS.
- The website tab appeared for every RGW and failed with a bare 500 on a default gateway.

**Diagrams updated:** `c2-containers.md` (storage text).

## 2026-11-07: File share exports follow the project's addresses

**What changed**

- Every release of a public address now passes through one guard (`PublicIPRepo.release`), and network and subnet delete
  through the same one: the address leaves the project's exports first, under a per-project lock, and a NAS that refuses
  fails the release.
- The sync reconciles exports with the project's addresses. Project teardown deletes shares before the addresses go back.
- NFS exports squash root; a dataset is `777`. SMB account names are a hash, stamped on the dataset.

**Why**

- Exports were written only at create and on refresh, so a released address or CIDR reallocated to another tenant
  reached the first tenant's dataset as root. An address is only safe to reallocate once nothing lists it, and the row
  in `pvePublicIp` (or the SDN subnet) is what holds it, so that row goes last.
- The lock exists because a refresh during a release would write the address straight back. The sync reconcile covers
  exports that were already stale when this shipped, and anything edited on the NAS by hand.

**Diagrams updated**: `c3-components.md`.

## 2026-11-07: Protected servers and several API endpoints

**What changed**

- New `internal/cloud/pve/protect.go`: replication jobs, HA resources and node-affinity rules, gating, RPO state on the
  server row, `server_protection` billing, memory reservation in the scheduler, an operator migrate route.
- `config.apiUrls` and `config.caCert`: the PVE client fails over between endpoints and pins the cluster CA.

**Why**

- Local ZFS is pinned to one node; a site that needs a server to survive a node failure had no answer without Ceph.
- HA is added only after the first sync, since a failover before that has no replica to start from.
- Node-affinity rules with equal priority, not `/cluster/ha/groups` (gone in PVE 9): a recovered node must not pull the
  VM back on its own, a second interruption nobody asked for.
- A single `apiUrl` made the node hosting it a point of failure for the whole portal, exactly when HA is needed. Each
  node has its own certificate, so several endpoints need the cluster CA, not a fingerprint.

**Diagrams updated**: `c2-containers.md`, `c3-components.md`.

## 2026-11-07: P5 IaaS parity: server seeds, rebuild, snapshots, subnets, spread groups

**What changed**

- Server seeds (C3b): Planned -> Implemented. A server with user-data, vendor-data or instance metadata gets a
  platform NoCloud seed: a per-VM token URL on the API (`GET /api/v1/nocloud/{token}/{file}`) on a vnet with DHCP,
  a `cidata` CD written by the platform (`pve/seediso.go`) otherwise, or with `metadata.forceIso`. Any other
  server stays on PVE's own drive.
- New actions on a server: `SET_PASSWORD` (guest agent), `REBUILD` (boot-disk swap), `LIST_/CREATE_/ROLLBACK_/DELETE_SNAPSHOT`.
  New on a network: `ADD_SUBNET`, `REMOVE_SUBNET`, `SET_SNAT`. New on create: `spreadGroup`, `spreadStrict`, `metadata`,
  `networkInterfaces[0].subnet`.
- The client gets a public-IP card, a Snapshots tab, a subnets tab and the wizard inputs. The tab that created
  template images is now called Images, for what it does. All behind `/init` feature flags.
- `ProxCloudPlatform` adds `VM.GuestAgent.Unrestricted` and `Datastore.AllocateTemplate`.

**Why**

- A URL seed alone cannot work on EVPN or VLAN: cloud-init fetches it in the network stage, the local stage gives
  up on any CD once it sees a seed URL (lab-verified on cloud-init 25.1), and with no DHCP the guest then has no
  network to fetch with. A CD alone would put user-data on shared storage everywhere and need the upload privilege
  on sites that have DHCP. So the vnet decides.
- The ISO writer is ~150 lines of Go: `user-data` is not a legal ISO 9660 name, so the image needs Joliet, and the
  pure-Go options are whole disk-image libraries.
- Tenant data rides `vendor-data` and `meta`: on one key user-data wins, and a tenant metadata key cannot reach
  `dsmode` or `seedfrom`.
- Rebuild clones first and parks the old disk: a failed clone or move leaves the server as it was.
- A spread group is a tag and a scheduler preference because PVE 9's affinity rules are HA-only and strict.
- There is no router resource: the zone's VRF already routes all of a project's vnets and the isolation rules keep
  other projects out, so a router object would have nothing to decide.

**Diagrams updated**: `c2-containers.md` (Tenant VMs), `c3-components.md` (C3b).
## 2026-11-07: CI opens the built SPAs

Open item of the lab drill (DRILL-1). The same PR fixes console and quota bugs with no architecture
effect.

**What changed**

- New `spa-smoke` job in `test.yml`: builds each SPA, serves the production bundle and opens it in
  Chromium. A load error or a missing login page fails it.

- `ManagementCIDRs` leaves out a node that cannot answer and says so. `ReconcileIsolation` then keeps
  the DROPs in place and still sets the project's ACCEPTs.

**Why**

- tsc and vite build stay green on a bundle that renders a white page, which is how a react and
  react-dom split once reached a running console.
- One node down made every isolation reconcile fail with `595 No route to host`, seen live during a
  node outage. Network create and delete go through the same call, so they failed for every tenant.
  Dropping the failed node's networks instead would have opened a hole if only it knew one.

**Diagrams updated**: `c4-code.md` (C4.1 invariants).

## 2026-11-07: operator Create user makes a Keycloak login

Open item of the lab drill (DRILL-2).

**What changed**

- `POST /admin/user` creates the Keycloak account through `sso.KCAdmin`, keys the row by its id and
  returns a temporary password once. `DELETE /admin/user/{id}` removes the account too.
- Without the service account Create user answers 501 instead of writing a row.

**Why**

- The row had a random `sub` that no login could ever match, so the user could not log in and a later
  self-registration made a second row. Reusing the SSO service account adds no client and no secret:
  it already holds `manage-users` in the customer realm, on compose, the chart and the installer.

**Diagrams updated**: `c3-components.md` (note under C3a).
## 2026-11-07: API tokens get a scope and an expiry

**What changed**

- A personal access token has a scope, `full` or `read`, and an optional expiry. `pkg/auth.ClientKeyLookup`
  returns a `ClientKey`; a read token passes safe methods and the POST list route only.

**Why**

- P5-12 asked for platform API tokens; the tokens already existed. What was missing is limiting one: a token
  in a dashboard or a CI job should not be able to delete servers, or live forever. `/mcp` is refused for a read
  token because its tools can write.

**Diagrams updated**: none, `docs/auth.md` has the rules.

## 2026-11-07: volumes outlive their server (holder VM)

**What changed**

- New `internal/cloud/pve/holder.go`. A volume without a server sits on a per (project, node) holder VM; deleting a
  server moves its data volumes there by default, `?deleteVolumes=true` destroys them. Standalone create and delete.
- Holders are skipped by the SERVER sync and metering (no row, no bill, no quota) and by the scheduler; their disks
  stay VOLUME rows.

**Why**

- A Proxmox disk must belong to a VM, so "delete the server" used to delete data the tenant had detached on purpose.
  The UI warning that stood in for this is gone.
- Keeping volumes by default matches the safe reading of "delete a server": data loss needs an explicit tick.
- A holder per node because PVE reassigns a disk only within a node.

**Diagrams updated**: `c3-components.md` (PVE service).

## 2026-11-07: isolation rules get an owner, cover gateways, and reconcile in the sync job

Open items of the lab drill (DRILL-5, DRILL-6) and the coexistence note in the lab log.

**What changed**

- The sync job reconciles each project's isolation rules every cycle under the `sdn-apply` lock.
- A portal only rewrites DROPs with its own comment: bare `proxcloud:isolation` for the default
  pool prefix, `proxcloud:isolation/<poolPrefix>` otherwise. It only moves ACCEPTs inside its pool.
- `IN DROP` inside the pool, plus an `IN ACCEPT` per project next to its `FORWARD ACCEPT`.
- Rules are deleted by identity with the list's digest instead of by a remembered position.

**Why**

- Rules only reached a project at its next network change, so projects made before a rule change
  kept the old set indefinitely.
- A cluster normally has one portal, and that portal has to keep owning its rules outright so a
  changed pool or blocked list leaves nothing stale. The old code took that to mean every rule with
  the marker, so a second portal with another pool deleted the first one's DROPs at its next network
  change and left those tenants unisolated with no error. Scoping by owner keeps the one-portal
  behaviour and makes the second portal harmless. The default prefix keeps the old comment so no
  running cluster needs a migration.
- A gateway address is local to the node, so traffic to another tenant's gateway is input and no
  `FORWARD` rule sees it.
- PVE addresses rules by position, and a reconcile now runs without anyone asking for it, next to
  operators and possibly another portal editing the same list. A stale position would delete the
  wrong rule, possibly someone's DROP.

**Diagrams updated**: `c4-code.md` (C4.1), `c2-containers.md` (tables).
## 2026-11-07: TrueNAS client and file share product

**What changed**

- New `internal/cloud/truenas`: JSON-RPC 2.0 client over WebSocket and the share service. New provider kind `truenas`
  serving `SHARE` resources, with sync, teardown, a `share` billing resource and a "File shares" page.
- New dependency `github.com/coder/websocket` (ISC) for the client side of the socket.

**Why**

- TrueNAS 26 removes the REST API and revokes a key sent over HTTP, so a REST port would have had no future.
- Tenants on the Standard tier had no shared file product; the NAS is already there as the capacity tier.
- A share is one dataset per project, not the 1/N split across projects of the older design: per-project billing and
  quota come out exact, and no tenant ever shares a filesystem with another.
- Exports list tenant addresses because the NAS only knows a client by its source address. A private subnet behind
  SNAT arrives as a node address, which the NAS cannot tell apart per tenant, so it is left out of the promise.

**Diagrams updated**: `c1-context.md`, `c2-containers.md`, `c3-components.md`.

## 2026-11-07: RustFS tenant buckets behind objectBackend

**What changed**

- `internal/cloud/client`: `Client` now wraps an `objectBackend` with `Caps()`. `cephBackend` keeps RGW;
  the new `rustfsBackend` embeds its plain-S3 data plane. New provider kind `rustfs`
  (`ExternalService.ObjectClient` picks the backend); methods renamed to neutral names.
- RustFS ownership lives in the project's canned policy, the project quota in the API (sum of bucket quotas),
  and scanner usage keeps the last value when the scanner cannot answer.
- Extra S3 keys on RustFS are IAM users granted through bucket policies with a bare-name principal.
- Bucket billing: new attribute `quota_gb` next to `size_gb`; the priced attribute is the billing basis.
- The installer registers the RustFS it deployed as a `rustfs` provider at stage 60.

**Why**

- Standard-tier sites had a deployed RustFS but no tenant bucket product.
- RustFS has no bucket owner and no user quota, so isolation and quota have to live on our side.
- Lab check RUSTFS-POLICY-1 on 1.0.4 showed user principals do work in bucket policies, but only by bare
  name, so grants could keep RGW's model instead of moving to identity policies. The ARN form saves and
  silently matches nothing, which is why the principal form is per backend.
- A tenant able to delete a bucket directly would leave its ARN behind and reach the next owner of the
  name, hence the Deny on the project key and the clean-up of other policies at create.
- Scanner usage is sometimes unavailable; writing 0 would bill nothing for that interval.

**Diagrams updated**: `c1-context.md`, `c2-containers.md`, `c3-components.md` (C3 managed services).
## 2026-11-07: per-organization SSO (PR #45)

**What changed**

- New package `internal/platform/sso`: per-organization identity providers (OIDC and SAML), one
  Keycloak broker per org in the customer realm, created through the Admin API by the service
  account `proxcloud-idp-admin`. Token issuer, SPA and MCP stay as they were.
- Domains verified through DNS TXT, email-first discovery in the client console, JIT membership
  with group to role mapping, enforce-SSO with up to two break-glass owners, an operator list and
  emergency stop, org and operator console pages.
- `pkg/auth.Enforce` gets a guard hook. The SSO guard runs on every request and answers 403
  `sso_*`; on public paths a refused identity continues anonymous.
- `User.ssoAlias` binds a person to the IdP that provisioned them.
- Keycloak 26.8.2 in compose. `deploy/charts/proxcloud/files/keycloak-sso.json` is the realm
  fragment both compose and the chart apply with keycloak-config-cli. The installer turns SSO on
  and generates the service account secret.

**Why**

- Until now SSO meant one broker for every customer of the platform. One realm with a broker per
  org (plan option B) keeps a single issuer, so ADR 0003 holds and SAML comes for free.
- A tenant controls everything its IdP asserts, including the email claim on every login. That is
  why the rules sit in a per-request guard and not only in the JIT step: `users.Require` creates
  users on any route, and a rewritten claim must fail after the first login too.

**Deviations from the plan, found while attacking it**

- Domains: the unique index is on `verifiedDomain`, not `domain`. With a unique domain, anyone
  could squat a name they do not own by claiming it first.
- Account binding is new. Keycloak can end up with a foreign IdP linked to a victim's user (the
  victim follows an email link); the IdP then rewrites the email into its own domain and passes the
  domain gate on the victim's `sub`. The guard refuses an IdP other than the bound one, and for an
  unbound account checks the address ProxCloud already had. Binding state is read fresh: an early
  cached version let a brand-new account through.
- The guard has no exemptions for `/user` and `/account`. One rule is simpler, and the console shows
  the refusal from the 403.
- Enforcement also covers members of the org, not only addresses in its domains. A member could
  otherwise change their email to leave the rule. Enforcing needs a break-glass owner.
- `proxcloud-sso` is a marker scope: while any org has SSO configured, a token without it is refused.
  Without it a self-registered MCP client could drop the mapper and make a brokered session look
  like a local one, skipping the domain gate.
- `SweepForeignUsers` removes Keycloak accounts created by an IdP asserting a foreign address, which
  otherwise block the real owner at "account already exists". This adds `manage-users` to the
  service account. No `manage-realm`.
- Metadata is fetched by the API behind an SSRF guard and handed to Keycloak as a file, so Keycloak
  never dereferences a tenant URL at configuration time.
- No custom first-broker-login flow: Keycloak's stock flow has no automatic link, and the IdP pins it
  by alias. No `verifyEmail` in the chart realm: an install without SMTP could not register anyone,
  and `email_verified` already fails closed.
- Keycloak 26.8 needs the `kc-admin-api` scope on Admin API tokens. keycloak-config-cli has no 26.8
  image; `6.5.1-26.5.5` was verified against 26.8.2.

**Known ceilings**

- A foreign-domain Keycloak account can exist for up to 5 minutes until the sweep.
- Keycloak calls the token and JWKS URLs at login; they are checked when configured, not on each call.
- A domain stays verified if the TXT record is later removed.
- With operators in the customer realm (compose default), brokered sessions are refused on operator
  clients, but that layout is for development only and the API logs a warning.

**Diagrams updated:** c1-context.md, c2-containers.md, c3-components.md (C3a), c4-code.md (sequence d).

## 2026-11-07: payments port (Airwallex, gateway factory, bank-transfer submit, invoices, record payment)

**What changed**

- `payment` gets an Airwallex gateway next to Stripe, selected per payment integration by a
  gateway factory (`payment.NewGateway`). The `Gateway` interface gains refund polling,
  releasing the 1.00 card-verification hold and a credentials ping (the admin health check).
- Card registration is now a small authorization hold that is released once the card is saved.
  Collect creates and confirms the charge in two steps with an idempotency key.
- A deposit confirm claims the PENDING transaction atomically (PENDING to PROCESSING), so
  repeated or concurrent callbacks and the scanner credit once; a settle error hands the claim back.
- Bank transfer, customer side: submit (transfer date, bank reference, note, proof of at most
  2 MiB), cancel, and proof download; finance is mailed on submit. The admin list never carries
  the proof bytes; the proof streams from its own route.
- `billing` gets invoice numbers (`INV-<year>-<seq>`, stamped once on leaving OPEN), invoice and
  credit-note PDFs with the operator as issuer, billing contacts, and a PREPAID or POSTPAID
  payment mode (POSTPAID charges the saved card when a bill is issued).
- Operators record an offline payment on a SENT bill: `POST /admin/bill/{id}/record-payment`
  and the MCP tool `record_bill_payment`.
- Mail carries attachments (the invoice PDF); the `pgdoc` store gains an atomic counter and
  nested transactions join the outer one; four mail templates are added.
- Admin: Airwallex integration form, bank-transfer proof download, record-payment dialog,
  invoice numbers. Client: Airwallex Elements for deposits and cards, bank-transfer panel,
  payment mode, invoice recipients.

**Why**

- A second card gateway removes the dependency on one provider and covers customers Stripe
  does not serve; the factory keeps the services unaware of which provider an integration is.
- Callbacks are unauthenticated redirects. Reading the outcome from the gateway and claiming the
  transaction before crediting is what stops a replayed or raced callback from minting a second
  credit. There is no webhook endpoint, so there is no signature to verify; the gateway is the
  only source of truth.
- Wire transfers are common for business customers, and the operator needs the customer's
  reference and proof to reconcile one; recording a payment closes the loop for payments that
  never touch a gateway.
- Tax invoices need a gapless number and an issuer block; credit notes and recipients are
  required to refund and to reach accounts payable.

**Diagrams updated**: C1 (Airwallex now implemented), C3c (payment, gateway factory, billing,
invoice and credit note rows and nodes).

## 2026-11-07: P7 managed Kubernetes, DBaaS and split mode (branch feat/p7-k8s)

**What changed**

- Managed K8s create on Proxmox (C3e): Planned -> Implemented. New chart `proxmox-kamaji-cluster`; the create path
  resolves flavor, network, addresses and credentials, with one MachineDeployment per PVE node.
- DBaaS create on Proxmox (C3e): Planned -> Implemented. The ten missing actions and create-with-restore are ported;
  endpoints come from a MetalLB pool, replicas spread hard across zones, backups go to any S3 endpoint.
- Services cluster, Cluster API + CAPMOX, MetalLB (C2): Planned -> Implemented. Installer stage 50, `mode: split`.
- K8s node template pipeline (C2): Planned -> Implemented, with image-builder.
- proxmox-csi disks (`-pvc-` volumes) no longer sync as the project's volumes.
- Applications are listed per provider label, since one cluster can host both a kamaji and a dbaas provider.
- Databases moved to `pc-db-<projectId>` namespaces: their default-deny NetworkPolicy cut tenant control planes in
  `pc-<projectId>` off from their workers when both shared a cluster.
- Removed from the add-on menu until they are wired: node problem detector, monitoring, NVIDIA GPU operator, reloader.
- Node group autoscaling is refused, and GPU flavors for node groups.

**Why**

- Pinned pools rather than a cloud controller manager: PVE clones to local storage only on the template's node, so
  workers are pinned to a node anyway, and the pin gives the zone label without another token in the tenant cluster.
- The worker's route to the PVE API goes through a Service on the services subnet, since tenant vnets are blocked from
  the management network, and its CSI token is scoped per cluster because a tenant can read it.
- The manager-level CAPMOX credential stays empty because it silently overrides `credentialsRef`: that is what
  upstream issue #513's error turned out to be here. The cloud-init check is off because on PVE 9 it needs
  `VM.GuestAgent.Unrestricted`, which is root in every guest.

**Diagrams:** C2 (containers, deployment), C3e.

## 2026-11-07: P4 metering foundations (branch feat/p4-metering)

**What changed**

- Proxmox state sampler (C3c): Planned -> Implemented. `pveStateSync`, every minute: one
  `/cluster/resources` page per provider reconciles SERVER, IMAGE and FLOATING_IP rows of every
  project, carrying agent IPs and the GPU count from the cached row.
- Proxmox task poller (C3c): Planned -> Implemented. `pveTaskPoll`, every 20 seconds, per-node
  cursor, UPID dedupe; re-syncs the project, audits other users' tasks, and backs `LIST_EVENTS`
  (was an empty stub).
- VM identity: portal-created VMs carry a `pc-c-<unix>` tag. A row whose identity changed is
  archived and a new one inserted in the same pass, disks included, so a reused vmid gets a new
  row and bill item. The portal also archives a row older than its own create.
- Traffic (C3c): Planned -> Implemented. `metrics.PVEFetcher` reads public-NIC counters from
  `status/current` every minute into `nicCounter` with reset-aware deltas. `pve` is the new default
  source of a Proxmox provider.
- Public IP and image billing: Planned -> Implemented. FLOATING_IP rows derive from the allocations
  (one per allocation, deletable, which detaches the NIC); new `image` resource type on `size_gb`.
- GPU count comes from `hostpciN mapping=` on the full sync, tags being only the fallback.
- Rating engine: a started unit is a billed unit, `getTimeUnitsDiff` floors at 0, rows archived this
  cycle are fed back with `DeletedAt`, `OVERWRITE_TOTAL` is stored bare on the first charge too.
- Staleness policy (C3c): new. Last-known state for 30 minutes, then the provider's resources stop
  billing, the gap is written off (`SkipInterval`, new on the `billingapi` wire contract), and an
  `ERROR` log line and a `METERING_STALE` audit event fire once.
- Admin API refuses a rule pricing a usage attribute unless it is `OVERWRITE_TOTAL` on an hour or
  minute cadence (an unnamed apply method defaults to overwrite).
- Removed: the inert `load_balancer` price type (provider, catalog entry, seed rule).

**Why**

The cache was refreshed every 15 minutes and keyed on the vmid alone, so state changes outside
the portal lagged charges, and PVE's reuse of the lowest free vmid let a new VM inherit the old VM's
row and bill item. Hour rules lost the first partial hour and the last interval of every
resource. One page per provider instead of 3N calls makes a 60-second cadence affordable, and
the 2-minute grace exists because pmxcfs lags: without it a just-created VM would be archived and a
just-deleted one resurrected. The stamp replaced `meta: ctime` as the identity because a clone keeps
its template's ctime (verified on PVE 9.2), so ctime only separates VMs created from scratch. A
started hour bills in full so that a resource's bill is a simple count of clock hours; truncating
would make any VM younger than an hour free. The write-off exists because `NotEligibleForBilling`
alone only delays a charge: the watermark would stay and recovery would back-bill the whole outage.
Status/current was picked over a metric-server push because it needs nothing installed on the
nodes; its ceiling is the traffic between the last sample and a counter reset.

An external billing service must mirror the rating changes (`getTimeUnitsDiff`, the first
`OVERWRITE_TOTAL` charge, `SkipInterval`).

**Diagrams updated:** C3c, C3b table.

## 2026-11-07: tenant isolation moves to cluster firewall rules, EVPN gets platform IPAM

Found by the end-to-end lab drill on PVE 9.2.

**What changed**

- Tenant isolation is now a managed rule set in the cluster firewall (`proxcloud:isolation`
  rules, one ACCEPT per project) instead of per-vnet firewall rules. It also drops traffic
  from the pool to the nodes' management networks and a new `network.blockedCidrs`.
- On a zone without DHCP (EVPN, VLAN) the platform allocates each server's `net0` address,
  sets it through cloud-init and pins `ipfilter-net0` to the `/32`. The allocation rows live
  in `pvePublicIp` on NIC `net0`; only `net1` rows count against the `floatingIps` quota.
- A public NIC takes the default route; attach and detach answer `rebootRequired`.
- `ResolveVM` reads power state and config from the node, not from `cluster/resources`.
- A failed project deletion stays `DELETE_IN_PROGRESS` and is retried, and `DELETE
  /project/{id}/now` now actually deletes.

**Why**

- Vnet `FORWARD` rules only see frames switched inside one vnet bridge. Traffic between
  subnets is routed by the node, so with the old rules one tenant reached another, and
  through an exit node's leaked VRF route also the node management network and storage.
- PVE refuses `dhcp` on an EVPN zone: network create failed outright, and DHCP is simple-zone
  only. A static address also lets ipfilter pin the exact address, which closes the
  spoof-a-sibling ceiling.
- With a default route on both NICs, replies from a public address left through the overlay.
- `cluster/resources` lags a finished task by seconds, so a resize right after a stop
  restarted the VM.
- A rollback to `ENABLED` after a half-done teardown showed a broken project as healthy and
  was never retried.

**Diagrams updated**: `c4-code.md` (C4.1, C4.2, sequences).

## 2026-11-07: P6 installer and image pipelines

**What changed**

- Installer (C2c): Planned -> Implemented. `installer/` is one Ansible entrypoint,
  `make install INVENTORY=...`, in `allinone` mode: preflight, Proxmox identity, NAS stores,
  OS templates, k3s (`mgmt.placement: ssh | pve-vm`, always `--cluster-init`), the platform
  through ArgoCD Applications with inline values, the object store, and registration through
  the admin API. Day-2: `upgrade`, `templates`, `storage-rebench`, `add-node`, `remove-node`,
  `mgmt-scale`, `rustfs-upgrade`, `rustfs-rollback`. `split` mode stays Planned with P7.
- VM template pipeline (C2c): Planned -> Implemented. `images/build.py` (vendor image, Packer,
  smoke test, publish by tag, prune) runs from the installer and from
  `.github/workflows/vm-templates.yml`, which has no PR or fork trigger.
- Template discovery (C3b): new. The catalog comes from cluster tags (`pc-public`,
  `pc-img-*`, `pc-v-*`) in pool `pc-templates`; `config.templates` is now an optional pin or
  overlay. Server create clones the target node's own copy, so a local-disk site is no longer
  pinned to one node.
- `proxcloud-api admin-key create`: new. A non-interactive Admin-API key, minted through the
  database; the one writer path (`admin.NewAdminAPIKey`) is shared with the endpoint and the
  debug trigger.
- Chart: CloudNativePG and a new `RabbitmqCluster` are the defaults, the bitnami subcharts
  are opt-in (`values-dev.yaml`). `cnpg.keycloak` and `auth.*.discoveryUri` added.
- RustFS (C2): Planned -> Implemented as the internal object store; tenant buckets stay
  Planned. Platform-owned VMs (C2b): Planned -> Implemented, except the services cluster.
- New `ProxCloudImages` role and `images@pve!packer` token.

**Why**

- A first install took hand-run `pveum`, hand-made templates and psql seeds. The plan's
  v3.1 decision is one command with the easiest default for every choice, each overridable.
- Tags, not config, because a weekly rebuild changes template ids: a catalog in config goes
  stale on every run. The pool is the ownership proof, a tag alone is not.
- Per-node copies, because PVE clones across nodes only from shared to shared storage (the
  P3 lesson): with local VM disks, one template meant every VM on one node.
- `ProxCloudImages` uses `Sys.AccessNetwork` instead of `Sys.Modify` on `/`, and has no
  ACL on `/vms`, so the pipeline token cannot touch a tenant VM.
- k3s's bundled Traefik and ServiceLB replace MetalLB and nginx-gateway-fabric in `allinone`:
  one host needs no VIP pool, and every extra component is one more thing to pin and upgrade.
  They return with the services cluster.
- cert-manager is always deployed, not only for ACME, because the RabbitMQ operator's webhook
  needs it (operator v2.23).
- The operator's own Secret carries the RabbitMQ credentials, so there is one less secret
  to generate and rotate. The API reads it directly.
- The admin realm is separate from the customer realm: any login in the admin realm is an
  administrator, and the customer realm allows self-registration.

**Diagrams updated:** c1-context.md, c2-containers.md (C2b, C2c, tables), c3-components.md (C3b)

## 2026-11-07: console and server detail match what Proxmox serves

No component changed. The wizard honors the boot volume type, offers the project's own
images and sends one network; the serial console speaks the `termproxy` framing; PVE write
calls run on a context detached from the client connection, because a browser going away
mid-clone left a half-built VM. Fields the backend ignored were removed rather than kept
(fixed IP, DNS and gateway on networks, volume name, security group description, ether type).

## 2026-10-28: P3 storage foundation and honest UI (branch feat/p3-storage, PR TBD)

Summary of the P3 slices below (P3-1 to P3-10); the per-slice entries carry the detail.

**What changed**

- Capability detection (C3b): Planned -> Implemented. `pve/capability.go` derives each
  storage's type, sharing, content, snapshot/thin/live-migrate/replication support, tier,
  default class and per-node free space from the cluster; it runs every sync cycle, on admin
  Re-detect and on every profile save. Admin class override per storage; the installer's
  fio result is a suggestion only.
- Storage profiles are validated against detection (flags may only downgrade; parsed flags
  are also clamped to the detected capabilities on every read, so a profile written on
  provider create cannot claim more), gain a
  derived tier and class, a `label`, and `bootDisk` / `dbaas` caps. A capacity-class
  profile never takes boot disks, databases or snapshots.
- Server create: placement restricted to nodes where the profile's storage is active and
  keeps `storageHeadroomPct` (default 10 %) free; a template on shared storage clones to a
  picked node whose profile storage is shared too (PVE clones cross-node only shared to
  shared), a local template or a local profile pins to the template's node; refusals are
  409 with the reason.
- Volumes: add-volume free-space check, same-node attach gating (PVE's disk reassign needs
  both VMs on one node), `LIST_ATTACH_TARGETS`,
  `RETYPE` via `move_disk`, delete of detached disks; per-disk `profile` and
  `storage_class` reach billing.
- Client: `/init` `features` per provider and `platformConfiguration.hiddenServices`; the
  client hides actions the backend would reject and warns on capacity types and on server
  delete.
- Docs: `cloud-integration.md` (new *Storage capabilities and placement* section, provider
  document, accessors, sync job, create sequence), `configuration.md` (provider storage
  keys), `data-model.md`, `glossary.md`, `architecture.md`, the admin guide
  *Connecting a Proxmox VE Cluster* (new *Storage* section) and the client guides
  *Block Volumes* and *Working with Servers*.

**Why**

- Cross-node clones from a template on local storage, and cross-node attaches of local
  disks, failed inside Proxmox with raw 502s after the platform had already committed to a
  node.
- The scheduler ignored free space, so a nearly full pool was picked and the clone failed
  late (or filled the pool).
- The client offered actions every Proxmox request rejected or silently ignored (rebuild,
  rescue, set password, user-data, boot-time data volumes, ports, subnets, volume
  snapshots, attach to any server).
- Nothing stopped tenants from putting boot disks, databases or snapshots on the HDD NAS,
  whose RAIDZ HDD plus sync NFS gives single-disk IOPS, and operators could not price it
  apart.

**Diagrams updated:** c2-containers.md (storage tiers table and text), c3-components.md
(C3b capability detection, Scheduler, PVE service, Sync job, Provider config, Project
cloud handlers, Platform config, Billing resources, DBaaS rows), c4-code.md (sequence a:
create server)

## 2026-10-28: Capacity HDD (NFS) profile and capability gating (P3-9, S18)

**What changed**

- Storage profiles gain a derived `tier` (full only when every storage is Ceph) and
  `class` (the lowest effective class of their storages), an optional `label`, and the
  `bootDisk` / `dbaas` caps (`false` downgrades). A capacity-class profile never takes
  boot disks, databases or snapshots; a profile save claiming them is refused (400).
- API gates: server create on a profile without boot disks is 409; `RETYPE` of a boot
  disk onto one is 409; an image (server snapshot) of a server with an attached disk on
  capacity storage is 409; a database create whose storage class is named after a
  profile closed to databases is 409.
- The volume sync stamps each disk's own profile and storage class (`volume_type` now
  follows the disk's profile, not the VM's). The volume billing resource exposes
  `profile` and `storage_class`, so price rules can price "capacity" separately.
- `LIST_VOLUME_TYPES` returns `displayName`, `tier`, `class`, `bootDisk` and `dbaas`.
  Client volume create and retype label capacity types "HDD, not for databases" and
  warn; the admin profile editor flags capacity profiles.

**Why**

- The HDD NAS is the Standard tier's capacity store, never the hot VM-disk tier: RAIDZ
  HDD with sync NFS gives single-disk IOPS. Tenants must not land boot disks, databases
  or snapshots there, and operators need to price it apart.

**Diagrams updated:** c2-containers.md (storage tiers table and text), c3-components.md
(Provider config, Billing resources and DBaaS rows)

## 2026-10-28: Honest client UI and managed-service switch (P3-6, P3-7, P3-8)

**What changed**

- `GET /init/{projectId}` gains `features`: per attached provider id, the optional actions
  its backend serves (`cloudFeatures`). Proxmox serves `serverSnapshots` (image from a
  server) and `volumeRetype` (only with two or more storage profiles). No backend serves
  rebuild, rescue, set password, user-data, boot-time data volumes, ports, subnets or
  volume snapshots yet, so the client hides them.
- `platformConfiguration.hiddenServices` (service slugs) is dropped from the `/init` menu,
  so the client hides Kubernetes, Databases or Object storage entirely. The admin
  System > Platform page has the toggles; providers keep syncing.
- Client: server delete dialogs warn that every volume of the server is deleted, detached
  ones included; Create volume picks the server; Attach lists `LIST_ATTACH_TARGETS`;
  Delete shows only for detached volumes; the server Network tab shows the guest-reported
  addresses when the provider has no ports.

**Why**

- The client offered actions every Proxmox request rejected (or silently ignored, such as
  user-data and boot-time data volumes), and operators had no way to hide a managed service
  that is not ready to sell.

**Diagrams updated:** c3-components.md (Platform config and Project cloud handlers rows)

## 2026-10-28: Volume attach gating, retype and delete (P3-4, P3-5)

**What changed**

- Attach to a server on another node is refused with 409: qemu-server's disk reassign
  (`move_disk target-vmid=`) dies with "Both VMs need to be on the same node" even on shared
  storage. A target with no free disk slot is 409 too. New volume action
  `LIST_ATTACH_TARGETS` returns the servers on the volume's node (none while it is
  attached).
- `RETYPE` (`newType` = profile slug) moves the disk with `move_disk storage=<profile
  storage on the VM's node> delete=1`. The target must hold VM disks, be active on that
  node and keep the headroom free; otherwise 409. The old cache row is archived, the sync
  adds the new volid.
- `DELETE` of a volume destroys a detached disk (drops its `unusedN` entry); an attached
  one answers 409 "Detach the volume before deleting it". Volume create still needs
  `serverId` (400).
- New `pve.ConflictError`, `Service.AttachTargets`, `Service.RetypeVolume`,
  `Service.DeleteVolume`; handlers map `ConflictError` to 409 (`pveVolumeErr`).

**Why**

- Cross-node attach of a local disk failed inside PVE with a raw 502, there was no way to
  learn which servers were eligible, and detached disks could neither be deleted nor moved
  to another volume type.

**Diagrams updated:** c2-containers.md (storage tiers text), c3-components.md (PVE service
row)

## 2026-10-28: Template placement and capacity-aware scheduling (P3-1, P3-2)

**What changed**

- Server create reads live storage capabilities and the template's config. A template whose
  volumes (disks, EFI/TPM state, cloud-init and ISO drives) all sit on shared storage is
  full-cloned with `target=<picked node>` and `storage=<profile storage on that node>`
  only when that profile storage is shared as well: qemu-server's clone dies with "can't
  clone to non-shared storage" for a cross-node clone onto local storage. A template with
  any volume on local storage, or a local profile, is pinned to the template's node; when
  the storage profile is not active there the create answers 409 with the reason instead
  of a 502 at clone time.
- Scheduler (`PickNode`) skips a node whose profile storage is inactive or would keep less
  than `config.storageHeadroomPct` (default 10 % of its capacity) free after the root disk.
  Data-volume create (`AddVolume`) runs the same check on the server's node and answers 409.
- `PVEStorageProfile.Nodes` + `pve.ActiveStorageNodes` replaced by
  `PVEStorageProfile.Placement(caps)` (node -> storage state); new `pve.StorageHasRoom`,
  `pve.TemplateShared`, `pve.ErrStorageFull`; `AddVolume` takes the headroom.

**Why**

- Cross-node creates from a local template failed inside PVE with a raw 502, and the
  scheduler ignored free space, so a full pool was picked and the clone failed late.

**Diagrams updated:** c2-containers.md (storage tiers text), c3-components.md (Scheduler
row), c4-code.md (server create sequence)

## 2026-10-28: Storage capability detection (P3-3, P3-10 API part)

**What changed**

- Capability detection (C3b): Planned -> Implemented. New `internal/cloud/pve/capability.go`
  derives per storage, from `GET /storage` and `GET /cluster/resources?type=storage`: type,
  shared, content, snapshots, thin, live migrate, replication (`zfspool` only), tier
  (`full` for rbd/cephfs, else `standard`), a default class from a static type table, and
  per-node active/total/avail.
- Provider config gains `config.capabilities` (written by detection: every services-sync
  cycle, the new admin `POST /admin/service/{id}/storage/detect`, and every storage-profile
  save) and `config.storageClasses.<id>` (`class` = admin override, set through the new
  `PUT /admin/service/{id}/storage-classes`; `suggestedClass` = installer fio result, shown
  only).
- Behaviour change: a `storageProfiles` save on a Proxmox provider is validated against a
  live detection (storage exists, active on its node(s), has `images` content, flags only
  downgraded). A cluster that cannot be reached rejects the save.
- Behaviour change: `PVEStorageProfile.Nodes` takes the active storage map; `*` now means
  "nodes where the storage is active" instead of "all nodes", and explicit node entries are
  dropped when their storage is inactive there. A server create on a profile with no active
  storage answers 409 instead of trying a node that cannot hold the disk.
- Admin UI: provider Storage tab (capabilities, active nodes, detected/suggested class,
  override select, Re-detect button).

**Why**

- Profile flags were admin-declared and unchecked, so a profile could claim shared or
  replication on local storage, or map a storage that is disabled on a node, and placement
  failed late at clone time. Detection gives the honest baseline the P3-4 attach gating and
  P3-9 caps enforcement build on.

**Diagrams updated:** c2-containers.md (storage tiers), c3-components.md (C3b diagram and
table), c4-code.md (server create sequence)

## 2026-10-28: Remove OpenStack remnants

**What changed**

- Dead OpenStack code paths are deleted: the gophercloud dependency, the Swift half
  of the object-store client, the `OS_*` / `openstack.*` config keys (the default
  region is now `PROXCLOUD_DEFAULT_REGION` / `proxcloud.default-region`, default
  `default`), the stale `/api/v1/notifications/` public-path entry, and the
  OpenStack-only client, admin and mgmt routes (`/debug/cloud`, metadata, service
  auth and details, discovery, project imports, unassociated projects, placement,
  reseller, availability zones, share protocols, VHI-ostor, Gnocchi granularity,
  volume-type writes) with their MCP tools and admin UI tabs.
- Persisted names now match the backend: table `gnocchiMetrics` is `usageMetrics`,
  job `gnocchiMetricsFetch` is `usageMetricsFetch`, download types
  `OPENSTACK_SWIFT_BUCKET` / `OPENSTACK_CONSOLE` are `OBJECT` / `CONSOLE`
  (`OPENSTACK_S3_OBJECT` is gone), the Gnocchi metrics source and the two Ceilometer
  Prometheus schemas are removed (only `libvirt-exporter` remains, pending a Proxmox
  schema).
- Public Admin API: `configuration.cloud.provider` reports the real provider kind
  (`PROXMOX`, `CEPH_S3`, `KAMAJI`, `DBAAS`) with a `proxmox.pool_prefix` block;
  `provisioned_services[]` carries `provider`, `region` and `pool` instead of the
  `openstack` block, and the `openstack_project_id` filter is gone.
- Managed K8s and DBaaS (C3e): the create-path stubs are renamed
  `cloud_kamaji_create.go` / `cloud_dbaas_create.go`; Cinder CSI, Octavia
  annotations, keystone application credentials, Glance image ids and neutron RBAC
  plumbing are removed, with one `TODO(P7)` at each seam naming the Proxmox piece
  (CAPMOX, proxmox-csi, MetalLB). Read, sync and delete are unchanged.
- CI: the rebrand gate also fails on OpenStack-era terms (this file and
  `countries.json` excluded).

**Why**

- **Misleading operators and API clients.** Dead paths and OpenStack wire fields
  (a fixed `OPENSTACK` provider value, empty `openstack` blocks, admin tabs and MCP
  tools that changed nothing on Proxmox) suggested features that do not exist and
  hid the real provider, pool and region. Names now match the Proxmox backend.
- **No installed base.** There are no deployed installs, so tables, jobs and enum
  values are renamed outright, with no compatibility shims.

**Diagrams updated:** c3-components.md (C3e stub file names). No diagram shape
changed.

## 2026-10-28: PR #16 tenant isolation, VM firewall, quota hardening

**What changed**

- Tenant networks (C2b, C3b, C4.1): every vnet now carries managed forward rules
  that accept the project's own subnets and drop anything else headed into the
  provider's `cidrPool`. New isolation mode `evpn-per-project` gives each project
  its own EVPN zone (id `ZoneIDFor(projectId)`); `shared` stays the default.
- Network create (C4 sequence b): the CIDR is picked under the platform `sdn-apply`
  lock, must sit inside `cidrPool` and must not overlap any subnet on the cluster.
  Isolation rules are written before the SDN apply. `cidrPool` is now required on
  Proxmox providers (breaking).
- VM firewall (C4.2, sequence a): `firewall=1` on every NIC, ipfilter and macfilter
  anti-spoof, inbound default DROP, a per-project `default` security group created at
  bootstrap. Cloned NICs beyond `net0` are removed. The firewall is set before the
  first boot; a failure destroys the VM. Server create now requires one of the
  project's networks (breaking).
- Public IPs (C2b, C3b, C4.2): new public IP pool per public network (`pvePublicIp`
  table) with a `floatingIps` quota; the address is pinned by ipfilter; the
  management bridge is refused.
- Test connection (C2 trust boundaries): refuses a cluster with the datacenter
  firewall off or a node on the legacy iptables firewall; warns on missing token
  privileges and on `root@pam` tokens. Least-privilege roles documented in
  `docs/proxmox-permissions.md`.
- Quota (C3b, C4.3): the provider default quota is seeded onto new projects; the
  gate fails closed (503) when usage cannot be read; resize, volume create and
  extend, and the boot disk are counted.
- Teardown (C4 sequence c): project teardown also removes the project's vnets,
  isolation rules, EVPN zone and security groups, and releases its public IPs. The
  scheduled deletion job now runs the same teardown (it used the old cloud write
  path, which never reached Proxmox resources).
- Identity (C3a, C2 trust boundaries): `email_verified` is read from the token
  and mirrored to `emailConfirmedAt`; org add-member and project invites match
  IdP-verified emails only.

**Why**

- **Cross-tenant reachability.** All tenant vnets shared one SDN zone with no rules
  between them, so a tenant VM could reach another tenant's VMs. The zone routes
  between its vnets; only firewall rules (or a separate VRF plus rules) stop it.
- **Spoofing.** Without `firewall=1`, ipfilter and macfilter, a VM could send with
  any source address, including another tenant's or a public address it did not
  hold. A template made from a VM with a public NIC would also have handed that NIC
  and its address filter to every clone.
- **Unbounded and untracked resources.** Public addresses had no pool and no record
  of who held them, and a failed usage read let a create through. Without a default
  quota a new project could consume the whole cluster. Quota now bounds what is not
  yet billed (public IPs, templates) and fails closed.
- **Account takeover through unverified email.** Anyone can register an account with
  an email address they do not own. Matching people by unverified email let such an
  account be added to an organization or claim a project invite meant for the real
  owner.
- **Silent misconfiguration.** With the datacenter firewall off, or a node on the
  iptables firewall, every rule above is silently unenforced; the preflight turns
  that into an error.

**Diagrams updated:** c1-context.md, c2-containers.md (C2b, trust boundaries),
c3-components.md (C3a, C3b), c4-code.md (C4.1 to C4.3, sequences a, b, c).

## 2026-10-28: Initial C4 model (baseline: initial ProxCloud release)

**What changed**

- Baseline of the model as of the initial ProxCloud release: a multi-tenant
  private-cloud portal and billing system for Proxmox VE. One Go API
  (`/api/v1`, SigV4 `/admin-api/v1`, `/mcp`, SSE, management port), customer and
  operator React consoles, PostgreSQL (JSONB document store), RabbitMQ, Keycloak,
  Helm charts and GitHub Actions CI publishing to ghcr.io.
- The IaaS backend is Proxmox VE (`internal/cloud/pve`): pools per project, full
  clones, SDN networks, cluster-firewall security groups, storage profiles, GPU PCI
  mappings. Tenant buckets run on Ceph RGW.
- Code base rebranded to ProxCloud (module, `PROXCLOUD_*` config, image names), with a
  CI gate that rejects old brand strings.
- Planned elements drawn from the roadmap: installer, Proxmox native metering
  samplers, HA and replication, TrueNAS and RustFS for the Standard storage tier,
  Proxmox Backup Server, per-org SSO, managed Kubernetes and DBaaS create on Proxmox,
  VM and node template pipelines, Airwallex.

**Removed (not drawn)**

- The OpenStack compute, network and identity backend; only the Ceph RGW bucket
  client survives in `internal/cloud/client`.
- The OpenStack notification relay (notifier); resource state now comes from the
  15-minute Proxmox sync.
- OpenStack-only managed-service legs: managed Kubernetes and DBaaS create on
  OpenStack are stubs that fail with an explicit error until the Proxmox versions land.
- Gnocchi as a metrics source; traffic metering uses an optional
  Prometheus-compatible source, off by default for Proxmox providers.

**Why**

- ProxCloud targets Proxmox VE private clouds. Keeping the OpenStack paths would
  have meant shipping code with no backend to run against; the bucket surface was
  backend-agnostic and stayed.

**Diagrams updated:** all four files created.
