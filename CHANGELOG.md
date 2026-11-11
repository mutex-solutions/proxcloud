# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Add native OS-template choices for Ubuntu 26.04 and Debian 12 while retaining
  Ubuntu 24.04 and Debian 13. New Kubernetes node builds select Ubuntu 26.04 by
  default, retain explicit 24.04 and verify the exact image-builder v0.1.55 commit.
  Bind OS target, seed, checksum, names and tags; export immutable upstream objects
  instead of trusting cached worktree bytes. Debian Kubernetes nodes remain unsupported
  by this pinned Proxmox route. Keep serial canary and cleanup/publication gates.

## [0.4.0-rc.1] - Unpublished preview

First broker-free ProxCloud release candidate, **not a stable release**. The owner
has authorized publication and released-artifact evaluation; preparation of this
section does not mean a tag, chart or image has been published or installed.
The five source security fixes cover SMTP credential TLS, stored-profile order
read authorization, custom-role assignment permission, native sandbox traffic
tokens and exact SSO alias retirement; details and limits are below. This is not
live installer (P6) or split/managed-service (P7) acceptance. Scheduling remains
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

Initial ProxCloud development line: a self-service private-cloud and billing portal
for Proxmox VE. No versioned release has been published yet; the first preview needs
coherent artifact/BOM pins and published-artifact installation acceptance. History
from the product this codebase was forked from is not carried over.
