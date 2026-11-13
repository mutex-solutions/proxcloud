# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Add **System → Template updates** to the admin portal: a GitHub integration holding a
  classic personal access token (`read:packages`, validated with GitHub before it is saved,
  encrypted and write-only) that every GitHub Packages call uses — no anonymous listing; the
  newest published VM artifact per OS/variant/Kubernetes line and the newest sandbox image per
  template, content-checked against the OCI and `manifest.json`/`SHA256SUMS` contract before
  being offered, next to what each provider has installed or pinned. A VM artifact can be
  imported by digest as a new Proxmox template (never modifying or deleting existing ones) via a
  fenced, resumable durable job, verified by a streaming SHA-256, Proxmox's own sha256 check and
  the layer digest; the executor is off until `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`.
  Sandbox image mirroring from the portal is not yet enabled; the page gives the `images.yaml`
  pin and the follow-up design is in `docs/template-updates.md`.
- Add a per-provider **attach policy** for new projects: `config.attachToNewProjects`
  (default `true` when absent, so every existing provider keeps attaching new projects). With
  it off, a new project's first bootstrap skips that provider. Set it on the provider's new
  **Projects** tab or with `PUT /api/v1/admin/service/{id}/attach-policy`, which changes only
  that key. A non-boolean value is refused there and on the provider create/connection save.
- Add **Attach to existing projects** (follow-up of #215): from a provider's Projects tab,
  attach it to all projects, the projects of picked organizations, or picked projects. It runs
  as a fenced, resumable PostgreSQL durable job (`providerattach.run/v1`), at most one per
  provider, with 1–4 projects at a time, and goes through the same path as the per-project
  attach. Already attached projects are skipped with no provider call; projects being deleted
  are skipped; suspended projects are attached and stay suspended. Results per project use
  fixed codes only. The tab shows progress, the failed projects and their reason, and offers
  cancel and retry-failed. Start, cancel and retry are audited (`BULK_ATTACH`, with actor,
  provider, scope and counts) and need `admin:service:manage`; reading needs
  `admin:service:read`. API: `/api/v1/admin/service/{id}/bulk-attach…`
  (`docs/cloud-integration.md`).

### Fixed

- The admin attach of a cloud provider to a project is now
  `POST /api/v1/admin/project/{id}/external-service/{externalServiceId}`; a `GET`, which
  provisioned the project on the provider, now answers 405 and changes nothing (#214). The
  attach stays idempotent: re-attaching a service the project already carries makes no
  provider call and adds no second binding. Scripts calling the old `GET` must switch to `POST`.
- A project cloud call that names a service (`x-service-id` or `?serviceId=`) the project is
  not attached to is now refused with `400 Project is not attached to service <id>` before any
  provider is called, instead of silently running on the project's Proxmox service (#215: a
  BUCKET create for a newly registered S3 store failed on Proxmox). This covers creates,
  actions, deletes and the reads `quota-usage`, `gpu-capacity`, `public-networks` and the S3
  key routes. Requests naming no service keep the Proxmox default. Existing projects still
  need an operator to attach a provider registered after they were created (per project, or
  with the bulk attach above).
- The per-project admin attach now refuses a project scheduled for deletion, being torn down
  or deleted with `400` instead of provisioning it. Attaches of one project, including two bulk
  runs for different providers and a concurrent first bootstrap, are serialized so none of
  them loses another's binding.
- `POST /api/v1/project/{id}/init` no longer answers `200 {}` when it leaves the project down.
  It answers `409` with `errors.details` `{status, reason[, billingProfileStatus]}`, reason one
  of `billing_profile_inactive`, `billing_profile_suspended`, `billing_profile_missing`,
  `disabled_by_operator`, `scheduled_for_deletion`, `deletion_in_progress`, `deleted`,
  `not_enabled` (#216). Success is unchanged. The client console shows the reason above the
  project's pages, with a link to Billing where that is the fix. The error envelope may now
  carry an optional `details` object.
- `POST /api/v1/project/{id}/cloud` with `{"type":"VOLUME"}` now answers the created volume —
  its cache row with `id`, `externalId` (the PVE volid) and `data.volume` (`name`, `size`,
  `status`, …) — instead of an empty `result` (on a server) or `{"result":{"node":…}}`
  (standalone), so the caller can use the volume at once (#218). If the new row cannot be read
  back yet the answer is the volume as the create knows it, without an `id`.

## [0.4.0] - 2026-11-12

First stable ProxCloud release, superseding the `v0.4.0-rc.1` prerelease. It is
broker-free: fresh installs need only a writable PostgreSQL primary for application
data and durable jobs, with no RabbitMQ. It builds with Go 1.27.4 and adds a
fresh-scan security gate. "Stable" here does not mean full acceptance.
Live installer (P6) and split/managed-service (P7) acceptance were **not**
performed; the owner deferred them. The job scheduler and debug triggers stay off by
default. Production mail delivery and the managed-sandbox saved lifecycle remain
held. Existing broker-era installations must explicitly drain/reconcile and retain
their RabbitMQ data before upgrading, because the new API does not consume old
queues. Under the security gate policy, HIGH/CRITICAL findings without an upstream
fix are tracked, not blocking, until 2026-12-12. Raw upstream bases are report-only.
The GPU/CUDA code-interpreter template is neither built nor scanned by the gate.
The `v0.4.0` tag publishes the images and app chart `0.4.0`. Verify that they can
be pulled before installing with `images.build: false`.

### Security

- Add a fresh-scan security gate (`security-scan` workflow) for every PR and `main`
  push. It checks out the exact head commit, not GitHub's synthetic merge commit. It
  uses a checksum-verified Trivy 0.76.2 binary and a vulnerability database that must
  be less than 24 hours old. Scan targets:
  - source lockfiles, including development dependencies;
  - six locally built product images: API, customer and admin consoles, sandbox
    proxy, runtime and importer;
  - eleven non-GPU sandbox template outputs;
  - thirteen deduplicated upstream base/builder/donor images, read as registry
    blobs and never executed;
  - a nondefault controlled-base prototype.

  Nothing is pushed or published. A required `security-gate` job needs every scan
  leg to succeed. On PRs, a decorated, size-bounded comment lists the CVE tables for
  each target.
- Apply the owner-approved gate policy (2026-11-12):
  - fixable HIGH/CRITICAL findings and end-of-life image OSes block;
  - HIGH/CRITICAL findings without an upstream fix are listed and counted as
    tracked, but do not block until 2026-12-12 (UTC); after that date they block
    again until re-reviewed;
  - raw upstream base findings are report-only, but missing, stale or invalid base
    scans still fail;
  - exactly one expiring VEX (until 2026-12-31) covers CVE-2025-47273, where
    setuptools 70.3.0 appears only in pip 26.3's vendored SBOM and the vulnerable
    module is not shipped.

  No other ignore, allowlist, suppression or severity change exists.
- Remediate fixable HIGH/CRITICAL findings in the eleven non-GPU built-in sandbox
  templates and ship the rebuilt templates as this release's digest pins. Every rebuilt template
  applies its distribution's available package updates. Per template:
  - base: Node 22.23.5 with npm 11.21.2, corepack 0.36.0 and yarn 1.22.22 replaces
    the obsolete Node/npm payload; pip 26.3, setuptools 84.2.1, wheel 0.48.0 and
    packaging 26.3 are upgraded;
  - node: moves to npm 11.21.2 (undici 6.29.1, brace-expansion 5.0.12);
  - browser: upgrades setuptools 84.2.1, virtualenv 21.15.6 (bundling pip 26.3),
    certifi, filelock, msgpack and urllib3, and drops the stale virtualenv seed
    cache;
  - code-interpreter: moves to Node 22, npm 11.21.2 and pip 26.3, and smoke-tests
    every installed kernel during the build;
  - mcp-gateway: installs its npm servers as one private project with
    `@modelcontextprotocol/sdk` 1.32.2 and `basic-ftp` 6.2.6 overrides, and the
    build checks every installed copy. It also refreshes the Node 22 donor and
    moves to uv 0.13.4.
- Refresh `libxslt` in both console images (CVE-2026-87636, CVE-2026-9947). Pin
  fixed transitive brace-expansion, fast-uri and systeminformation versions in both
  SPA lockfiles, and nanoid and source-map-js in the customer console. The sandbox
  runtime donor moves from BusyBox to an upgraded Alpine 3.24.4, so its BusyBox/musl
  packages are visible to scans.

### Changed

- Build and test with Go 1.27.4, the current stable release, in all six release Go
  builder stages (now `golang:1.27.4-alpine`) and the hosted test workflow. The module
  minimum rises to `go 1.26.0`, which Testcontainers 0.45 and `golang.org/x/crypto`
  0.58 require, so source builds with Go 1.25 no longer work. The compiler pin and
  the module minimum remain separate, tested declarations.
- Update Testcontainers core and PostgreSQL modules to 0.45.0 and `golang.org/x/crypto`
  to 0.58.0 (with `x/net` 0.62.0 and `x/sync` 0.24.0) as one coherent graph. Indirect
  modules move too, for example go-jose 4.1.6, docker/cli 29.2.0, `x/sys` 0.49.0 and
  `x/text` 0.43.0. pgx 5.12.0 and the AWS SDK versions already on `main` are kept.
  This supersedes the separate dependency updates in #182, #183 and #32.
- Move the API runtime image from Debian bookworm-slim to an upgraded Alpine 3.24.4.
  The image still runs as UID 10001, and the Go binary stays static. It ships bash
  and the PostgreSQL 18 `psql` client for config-as-code seeding with password or
  TLS authentication. That client cannot seed through Kerberos/GSSAPI; for that, use
  a suitable `psql` elsewhere, such as in the database pod.
- Run the tagged build-archive integration test in hosted CI.

### Fixed

- Grow only a freshly created native OS-template base to a verified minimum 20 GiB
  before template conversion or first-boot package provisioning. Preserve larger
  upstream disks, validate exact ownership/digest/readback and bound failure cleanup
  to the confirmed new VM; do not resize existing templates or tenant disks.
- Kubernetes node-template builds:
  - The final cleanup provisioner returns without shutting the guest down. Packer
    can then remove its temporary script before the Proxmox builder stops and
    converts the VM (#204).
  - After a successful build, only this run's final template is discovered. The
    search polls for at most 120 seconds and rejects pre-existing VMIDs and any
    ambiguous or mismatched candidate (#208).
- Before building, the OS, Kubernetes and sandbox template workflows disable any
  inherited sparse checkout and verify that the full source is present (#206).

### Added

- Export the Ubuntu 26.04 / Kubernetes 1.35.10 node template line through the
  public VM image pipeline as `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s`.
  The export has its own `--k8s-os ubuntu-2604` sanitizer path and
  `vm-k8s-export-ubuntu-2604-…` artifact name:
  - The only input is the raw 20 GiB root disk. Its hash is bound to the
    parent-reviewed source before any image parser runs. EFI vars and cloud-init
    media are never exported.
  - A pure copy policy checks a read-only inventory before any write. Unknown or
    unexpected facts make it refuse rather than delete. These include existing
    cluster state, swap, registry credentials, proxies, lab addresses,
    systemd/snapd secrets, unknown accounts and unreviewed cloud-init drop-ins.
  - Kubelet enablement and the exact Kubernetes packages are preserved.
  - The publisher refuses any other Kubernetes OS or version, and any mix of
    native and Kubernetes names and manifests.

  This release does not include runtime acceptance of the Kubernetes image (real
  inventory, sanitize, postscan, cold boot, anonymous pull). Its publication is a
  separate, manually dispatched step that the `v0.4.0` tag does not perform.

- Define portable native VM-disk QCOW2 artifacts for public GHCR distribution,
  with strict versioned manifests, checksums, unsigned exporter facts and explicit
  sanitization/pull/import/boot gates. Add a copy-only offline sanitizer and a
  manual sealed three-file staging → hosted ORAS publisher bridge, without giving
  the image runner PVE-root or package-write access. Publication plumbing does not
  imply a public artifact exists; package visibility and anonymous retrieval remain
  separately verified.
- Add evaluation-only security recipes. None of them is integrated into a default
  image or published, and none waives a scan finding:
  - an OpenSSH client backport (CVE-2026-60002);
  - a source-built GitHub CLI;
  - an MIT krb5 1.22.3 source build;
  - a nondefault controlled base-template prototype with five optional variant
    recipes.

  The OpenSSH, GitHub CLI and krb5 workflows run only on PRs that touch them and
  produce artifacts only.

- Add native OS-template choices for Ubuntu 26.04 and Debian 12 while retaining
  Ubuntu 24.04 and Debian 13. New Kubernetes node builds select Ubuntu 26.04 by
  default, retain explicit 24.04 and verify the exact image-builder v0.1.55 commit.
  Bind OS target, seed, checksum, names and tags; export immutable upstream objects
  instead of trusting cached worktree bytes. Debian Kubernetes nodes remain unsupported
  by this pinned Proxmox route. Keep serial canary and cleanup/publication gates.

## [0.4.0-rc.1] - 2026-11-11

First broker-free ProxCloud release candidate, **not a stable release**; published
as a non-latest GitHub prerelease. The five source security fixes cover SMTP
credential TLS, stored-profile order read authorization, custom-role assignment
permission, native sandbox traffic tokens and exact SSO alias retirement; details
and limits are below. This is not live installer (P6) or split/managed-service (P7)
acceptance. Scheduling remains
off, mail delivery and saved sandbox lifecycle remain held, and existing broker
data still requires explicit migration/retention evidence.

### Changed

- Offer NGINX Gateway Fabric 2.8.1 with Gateway API Standard 1.6.3 for new managed
  Kubernetes clusters (tenant chart 0.1.10). Keep the controller opt-in, users owning
  application Gateway/Routes/TLS, and existing ingress-nginx traffic unchanged.
  Validate actual chart/Kubernetes capability before effects, preserve sparse-client
  addon choices and refuse implicit legacy controller migration or downgrade.

- Refresh installation, developer, product-help, CI/release and support documentation
  against current configuration. Distinguish preview/source-build state from
  versioned publication and live acceptance, and document static template networking,
  scheduler defaults and intentional sandbox safety holds. No release is created by
  this documentation update.
- Replace application RabbitMQ dispatch with primary PostgreSQL durable native
  billing jobs: frozen original cycles, fenced at-least-once workers, atomic
  all-provider financial receipt/ACK, transactional enqueue, bounded retry/dead
  letters and audited operator replay. Reject old fanout flags until legacy queues
  are drained/reconciled; scheduler stays off by default. Optional read-only pool
  serves lag-tolerant history only. Fresh Compose/chart/installer defaults are
  broker-free; existing broker state still requires explicit drain/reconcile and
  retention. Bounded mail outbox machinery and opt-in enqueue producers are
  separate from production delivery, which remains disabled.

### Fixed

- Select Node 24.22.2 and stable NGINX 1.30.6 for both frontend image recipes,
  align the hosted Node patch, and guard the declarations in data-only CI tests.
  Preserve app locks, static serving and runtime public-config behavior; distinguish
  actual musl/runtime checks from source pins, image builds and live acceptance.

- Select supported Go 1.26.11 explicitly for all six Go builder stages and the
  test workflow, keeping the module language minimum and dependency graph separate.
  Add source-declaration regression coverage and document the seven output artifacts,
  external envd inputs, image-build gaps and remaining runtime acceptance limits.

- Update managed PostgreSQL 15–18 image-map patch targets to 15.19, 16.15, 17.11
  and 18.6 in unpublished database chart 0.5.1. New installer DBaaS providers
  offer 17/18 with 18 as default; existing name-matched providers and database
  chart pins/data remain untouched. Require actual offline Helm/catalog regression
  checks in CI; no existing PostgreSQL major upgrade is performed.

- Pin both frontend Vite builds to security-floor patch 8.1.6 and installer
  ansible-core to 2.18.18 without unrelated lock graph churn or live installer
  execution. These narrow fixes do not certify other dependencies or runtime images.

- Persist exact organization SSO alias retirement on the primary before broker
  deletion, and refuse retained sessions even when no configured providers remain.
  Keep legitimate realm-level social/local sign-ins compatible, make cleanup retries
  idempotent and document mixed-version, cached-rule outage and recovery limits.

- Bind client order reads to the stored billing profile's current organization
  read permission, return an opaque 404 when denied and serialize a typed order.
  Require role-management permission for all custom organization role assignments
  while preserving ordinary MEMBER invitations; reject unknown roles and malformed
  role requests. Private native sandbox services on port 49983 now require their
  traffic token; only actual envd-serving pods retain envd's own-auth exemption.
  Add native repository and proxy regression coverage; no held feature is enabled.

- Require verified configured-host TLS before legacy SMTP credential authentication,
  including LOGIN-only and AUTH-stripped relays. Guard LOGIN independently against
  plaintext and host mismatch; preserve explicit NoAuth relay compatibility and
  PLAIN preference. No outbox delivery, relay configuration or live send is enabled.

- Protect full and sampled Proxmox SERVER/VOLUME/IMAGE reconciliation from
  filtered, malformed or unreadable VM inventories. Same-credential scoped proof
  and whole-row CAS preserve cache/history on unknown reads; identity retirement
  requires complete existing ACL/node authority. Delay protection healing until
  proof succeeds. Orphan IP cleanup now requires authoritative VM/address non-use
  and exact allocation/archive receipts instead of age or bulk NIC release;
  ambiguous reuse, other guest NICs and legacy/BeforeRow allocations stay held.
- Fail closed on permission-filtered PVE NETWORK inventories: prove effective
  same-token visibility and revalidate omitted networks/subnets before cache
  writes, preserving tenant rows and NAS exports on unknown results while genuine
  delete-of-vanished still archives normally. Surface typed repair warnings.
- Refuse conflicting TrueNAS provider owners on the same project's NAS backend and
  parent dataset before share/export mutations or duplicate share caching, rather
  than letting different provider settings repeatedly rewrite the same exports.

Before `0.4.0-rc.1`, ProxCloud was an untagged development line: a self-service
private-cloud and billing portal for Proxmox VE. History from the product this
codebase was forked from is not carried over.
