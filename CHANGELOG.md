# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **The managed-services platform's bill of materials** (`deploy/platform/platform-bom.yaml`,
  `GET /api/v1/admin/platform/bom`): ADR 0009 says every release ships one list of the exact version and digest of
  everything the platform runs, so that the installer and the console's "Enable managed services" build the same
  thing and an upgrade has one source. The file names release `0.7.0-dev`, Kubernetes 1.35.10 with its published
  node image, the seed image and k3s v1.35.10+k3s1 (binary and install script by sha256), the Cluster API
  providers, the tenant charts and the 15 seed and workload components with wave, namespace, node group and chart.
  The API embeds it (`deploy/embed.go`; the image copies `deploy/embed.go` and `deploy/platform`, and the docker
  workflow rebuilds on a change to either) and validates it in `internal/platformlife`: exact versions only, a
  digest on every chart and image, and `pending` digests (our own four packages, not public yet, and the `kamaji` and
  `kamaji-crds` charts at `0.0.0+latest`, which have no stable digest) and `0.0.0+` versions only in a `-dev`
  release. The route needs `admin:service:read`. Tests keep `versions.yml`,
  `pc_defaults.services.k8s_version` and stage 40's ArgoCD and cert-manager chart and values equal to the BOM; the
  installer itself is unchanged and does not read the file. Inert: nothing installs from the BOM, there is no run,
  job or flag yet. Not verified: the third-party chart and image digests and the k3s sha256 values were read once
  from the public registries, chart indexes and k3s release on 2026-11-15, with no CI job checking them yet; whether
  `proxcloud-platform` 0.1.0, `proxmox-kamaji-cluster` 0.1.11 and `proxcloud-clusterctl` v1.14.4-1 get those
  versions depends on PRs that do not exist yet.
- **Every check of the Proxmox connection test in one answer**
  (`POST /api/v1/admin/service/proxmox/auth?checks=all`): the test stopped at the first failure, so an
  operator fixed the datacenter firewall only to learn about the node firewall, and then about missing
  privileges. With `?checks=all` the route answers `200` with `checks`, one `{id, status, message}` row per
  check (reachable, token, both firewalls, public networks, privileges per ACL path, the stored provider's
  store checks), and runs every check it still can. The call without the parameter is unchanged: the first
  failure is a `400` with the same message, which installer stage 60 reads. The body also takes `apiUrls`
  (failover addresses were tested only from a stored provider). With a stored provider and a blank secret,
  the fields the body carries now replace the stored ones for the test, so an edited form can be tested
  before Save; this needs `admin:service:manage`, because it sends the stored secret to the edited address.
  Such a test, when it sends the stored secret with an address, CA, fingerprint or `insecureTls` other than
  the stored ones, writes a `CONNECTION_TEST` audit event on the provider (actor, the fields that differ, the
  addresses tested, success or failure): the route is otherwise not audited and the provider does not change,
  so nothing else would say where the secret went. The event is built by a tested function; its write to the
  audit store was not run in a test. A read-only caller still tests the stored connection. `docs/api.md` lists the check ids. Tested against a
  fake PVE API only; not run against a lab cluster.
- **Setup checks in the Proxmox connection test** (`?checks=all` only): Proxmox VE 9.2 or later, the pool
  `pc-templates`, a storage with `import` content, and for a stored provider its default network zone,
  `cidrPool` (valid, and a warning when it overlaps a management network of the nodes or when a node's
  networks could not be read, so a node left out is not reported clear), default storage
  profile and, with an EVPN zone, `frr-pythontools` on every node. A new provider used to pass the test
  and then answer 503 on the first tenant network, or show no images, with nothing saying why. These rows
  never fail the default call and never add to `warnings`. A caller with `admin:service:manage` also gets
  the bridges of every node with a management flag, to choose a public network from, and, when the
  connection is refused over the certificate, the SHA-256 fingerprint the server presented, so it can be
  compared on the node and pinned. A read-only caller gets neither. A refused pin now reads
  `tls: failed to verify certificate: pve: certificate fingerprint mismatch` in the error text. Not
  lab-verified: reading a node's package list with the least-privilege token (the check warns when it is
  refused), and the type Proxmox reports for an SDN vnet in the bridge list.
- **A generated script for the portal token** (`GET /api/v1/admin/setup/proxmox-script`): a portal that was
  not set up by the installer needed the role, two pools, a user, a token and seven ACLs typed from
  `docs/proxmox-permissions.md`, and the usual mistake (an ACL on the user but not on the token) only showed
  as missing privileges later. The route answers one script as text, for a token ID, with or without the
  GPU mapping ACL and the import store ACL. It can be pasted into a root shell and run again: it runs in a
  subshell, single-quotes every value (the `!` of a token ID is history expansion otherwise), creates the
  role empty and appends its privileges, and ends with the token's effective permissions and
  `pvenode cert info`. The ACLs now live in one table next to the privilege list
  (`pve.PlatformACLs`); tests keep the script in `docs/proxmox-permissions.md`, the installer's role and
  ACL rows (stage 10) and that table equal, and refuse a token ID or storage that is not a plain id. The
  portal still holds no root credential. The script has not been run on a Proxmox node.
- **Defaults applied from the API** (`GET` and `POST /api/v1/admin/setup/defaults`). A new install needs a flavor
  catalog, a public price plan, a default project quota and a region row before a tenant can create a server. Until
  now that took a checkout, python3 and `deploy/seed/seed-operator.py` with four overridden variables on a Compose
  install. The API now carries the same `deploy/seed` documents (`deploy/seed/seed.go` embeds them; the API image
  copies `deploy/seed`) and applies them on request. The preview shows what would be written. The apply only creates:
  it never changes a document that exists, runs in one transaction under one advisory lock, and answers one result
  per item. The price plan is created enabled, so the apply refuses it without `"confirmRates": true`, and it is
  skipped when any enabled PUBLIC plan exists, because every enabled public plan is billed. When every such plan is
  scoped to other providers than `serviceId` the item is blocked and says so, instead of reading done while that
  provider is billed nothing; the setup status agrees. The plan gets the rules a
  Proxmox install bills; GPU, share and bucket rules are added only when such a provider exists. The default quota
  is merged under the stored one, so per-model GPU limits and limits the operator set are kept. Each item needs the
  permission of the endpoint that writes the same document by hand. `seed-operator.py` is unchanged.
  `docs/api.md` describes both routes. Not verified: a run against a Compose install, and the published image build
  (the build stage was built locally; CI builds the image).
- **Setup status from the API** (`GET /api/v1/admin/setup/status?serviceId=&live=1`). The Dashboard and the setup
  page need to know which step of a new install is done, and the row counts of `/admin/stats` call billing and
  branding configured on a fresh Compose install, because the seed writes those rows. One pure function computes six
  steps (connect, network, storage, templates, catalog, go live) with one item per check, from the database alone,
  and with `live=1` also from the cluster: every row of the connection test (`?checks=all`, the same function, not a
  copy of its checks), each in the step it is about (the zone, cidrPool, frr, public network and store reach rows
  in network, so a network problem does not send the operator back to the connection form; network and storage
  rows replace the database item that asks the same, keeping its id), the nodes with an active import store, the
  published OS images and the imported templates. A table test pins a fresh Compose install to no step done. Go live waits for connect, network, storage and templates, and is `unknown`
  while one of them could not be read (templates, without `live=1`), so the database-only answer never offers
  go live on a cluster nobody looked at; an open catalog and
  a job scheduler that is off are warnings, by the owner's decision, so they block nothing. Only enabled public
  plans that apply to the provider count as pricing it, and the quota counts as set only when it sets every limit
  the defaults would add, the same rule as the defaults preview. The route needs the four reads of the defaults
  preview.
  Not verified: a live run against a Proxmox cluster (the live checks run against the test fake).
- **Proposed storage profiles** (`proposedProfiles` in `POST /api/v1/admin/service/{id}/storage/detect`). The
  installer proposes profiles from the storages it finds (`classify_storages`); a console install had to write them
  by hand. The detect answer now carries the same proposal (`pve.ProposeProfiles`, tested on the installer's own
  cases), nothing of it stored. It skips a storage active on no node, claims a flag only when the storage has it, and
  claims no snapshots on a storage the operator set to capacity class (`storage-classes`), so the proposal passes the
  profile save unchanged. The rest of the answer is as before.
- **Template imports create pool `pc-templates` when it is missing** (`docs/template-updates.md`). A template import
  creates its VM in that pool, and on a cluster where nobody ran `pveum pool add pc-templates` the create failed. The
  job now reads the pool before the upload and creates it once, fenced like its other Proxmox writes; an existing
  pool is only read, so a token without `Pool.Allocate` keeps importing where the pool was made by hand. A token that
  may not create it fails the job as `pve_forbidden` before anything is downloaded. The connection test's
  `template-pool` row, which the setup status shows in the connect step, says so instead of sending the operator to a
  root shell for `pveum pool add`, which it keeps as the fallback for such a token. Not verified: that the
  least-privilege portal token can create `pc-templates` on a lab cluster (it holds `Pool.Allocate` on `/pool`, which
  creates the project pools).
- **Docker Compose install: opt-in VM template updates and job scheduler** (`deploy/compose/`): the API
  reads both switches only when it starts, and the Compose install had no way to set them, so on one host
  nothing was synchronised or charged on a timer and no template could be imported. `compose.yaml` now
  passes `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED` and `PROXCLOUD_JOBS_SCHEDULER_ENABLED` to the API from the
  `.env` keys `TEMPLATE_UPDATES_VM_ENABLED` and `JOBS_SCHEDULER_ENABLED`, both `false` by default.
  `./proxcloud-up.sh install --template-updates` and `--scheduler` write `true`, `--no-template-updates`
  and `--no-scheduler` write `false`. A later `install` without the flag and every `upgrade` keep the
  recorded value, so a re-run cannot turn billing off or on by omission; the last lines of `install`,
  also with `--dry-run`, say what is on. The script removes both names from the environment it hands to
  Compose: an install whose `.env` predates the keys cannot have its billing timers started by a
  variable exported in the calling shell. The scheduler needs nothing the stack does not have: its job
  locks (ADR 0007) and the queued charge work live in the stack's PostgreSQL. `deploy/compose/README.md`
  (Optional features) lists what each switch starts and what the scheduler means on one host. Turning
  the scheduler on does not mount the unauthenticated `POST /debug/*` job triggers on the management
  port: only `PROXCLOUD_JOBS_DEBUG_TRIGGERS` does (see the management port entry under Security), and
  this stack never sets it. That is true from the first release after v0.6.0: up to v0.6.0 the API
  mounted them with the scheduler too, so `install` and `upgrade` refuse to run such a release with the
  scheduler on (`install --scheduler` on a checkout still pinned to v0.6.0 included) and say why. The
  port stays unpublished and unrouted. The README points on-demand runs at the authenticated
  `/api/v1/admin/job/*` endpoints and names the jobs they cover; the bill, dunning and project deletion
  jobs run only on the timer, and `send-test-mail` is not available here. The README also says that `--no-scheduler`
  only stops new timed work: the API's job worker always runs and still processes the billing work
  already queued in the database, which matters when a backup is restored. **Not verified**: either
  switch on a running Compose install (the tests run the script with `--dry-run` and against a fake
  `docker`, and check the model `docker compose config` builds), a billing month with the scheduler on,
  the bill and reminder jobs without an SMTP integration, and how charging catches up after the host
  was down.
- **Platform setup page in the operator console** (`/setup`, also reached as `/onboarding`, where the API sends an
  install without a platform configuration; linked from System and from the Dashboard for an admin who holds the four
  reads the status needs: service, price plan, platform configuration and billing configuration). A new install was
  set up from four pages and a doc block, and the Dashboard checklist counted rows that the Compose seed writes, so it
  called a fresh install configured. The page shows one tab per step with the state `GET /api/v1/admin/setup/status`
  reports (the database answer at once, the live one when the cluster has answered; a state the API could not read
  shows Unknown, never done, and when a later read fails the page drops the answer it had instead of showing it as
  current), and the Dashboard card shows the same steps. Step 1 asks for the token ID, GPUs and an import store and
  shows the generated root script with a copy button and the two firewall switches. Step 2 takes the connection, runs
  every check of the connection test (`?checks=all`) with one row each, offers to pin the fingerprint the server
  presented only after the operator compared it with `pvenode cert info`, and saves the provider with
  `attachToNewProjects: false`, so no project is attached before Go live. Steps 3 to 8 are described below. Playwright
  specs against the mock API cover the script, the check rows, the pin, the create body and the axe checks; not run
  against a live API or cluster.
- **Network and storage steps of the Platform setup page.** A provider's network and storage were raw JSON on the
  provider page, and a wrong block showed up only as a failed tenant network. Step 3 is a form: zone kind (EVPN, VLAN,
  simple), zone ID, the VLAN tag range (1 to 4094, first not above last, which the API does not check), the project
  network range (`cidrPool`, default `10.100.0.0/16`, locked once saved because every tenant network is cut from it),
  EVPN peers and exit nodes from the cluster's nodes, an optional public network that only a bridge without a node
  address or default gateway on any node can carry, and the networks tenants must not reach. It saves with
  `PUT /api/v1/admin/service/{id}`, which replaces each top-level config key, so it reads the stored block first and
  sends it whole: public networks, isolation and other zones are kept. Step 4 detects the storage, shows the proposed
  profiles to confirm (a saved profile wins over a proposal of the same name), and saves the default profile, the
  import store and the backup store. Without a store that takes `import` content it shows the one `pvesm set` command;
  it also lists the three `pvesm add nfs` commands of a NAS (the stores installer stage 05 creates) and the
  prerequisites of a TrueNAS provider, with a link that opens Add provider on TrueNAS
  (`/system/cloud-providers?add=truenas`). Playwright specs check the saved bodies against the mock API; not run
  against a live API or cluster.
- **OS templates step of the Platform setup page.** Getting a first template onto a console install meant the
  Template updates page, the Integrations page and raw JSON for the smoke network, one image and one node at a time.
  The step is done once the provider publishes an OS image, however it was built, and then shows nothing more.
  Otherwise it shows whether template updates are on (if not, the installer command
  `./proxcloud-up.sh install --template-updates`, because the API reads the flag only at start), takes the GitHub
  token (`read:packages`; version listing has no anonymous access), saves the smoke-test network
  (`config.templateSmoke`), imports the picked OS images on the picked nodes in one go (one request per image and node,
  each staged and smoke-tested as before) with their progress, and has one **Publish all that passed** button that
  sends one publish request per template whose smoke test passed. Nothing is published without that click.
  Kubernetes node images are left to the Template updates page. Playwright specs check the flag-off state, the import
  requests and that publish is called once per passed template, against the mock API; not run against a live API.
- **Catalog, Go live and Next steps of the Platform setup page.** The defaults (flavors, public price plan, default
  quota, region row) needed a checkout and `seed-operator.py`, and going live meant the provider's Projects tab plus a
  sync nobody ran on a Compose install. Step 6 shows what `GET /api/v1/admin/setup/defaults` would write, the rate
  table included, before anything is applied; the price plan is applied only after the operator ticks that the rates
  were reviewed, and the apply sends `confirmRates` only then. It warns about flavors that no rule prices (billed as
  zero) and links the billing details and mail as recommended. Step 7 is enabled when Connect, Network, Storage and OS
  templates are done (a warning there does not hold it back, a state the cluster did not report does) in a live
  answer, and the catalog is done or the operator ticks going live without it. A database-only answer never enables
  it: there Connect is done once the provider is saved, without its connection checks. When the job scheduler is off
  it says that nothing then syncs or charges on a timer and shows `./proxcloud-up.sh install --scheduler`, without
  blocking. Go live turns the attach policy on, runs one provider sync
  (`POST /api/v1/admin/job/external-service/sync`) and starts a bulk attach of the projects created meanwhile when
  there are any, then lists the first tenant's path. Step 8 is a static card that links ADR 0009. `docs/quickstart.md`
  (path A2 and After any install) and `deploy/compose/README.md` now point at the page; the install flags
  `--template-updates` and `--scheduler` the page names are those of the Docker Compose entry above (opt-in VM
  template updates and job scheduler). Playwright specs check the preview before Apply, the confirmation in the apply
  body and Go live staying disabled until its prerequisites hold, against the mock API; not run against a live API,
  and Go live not run against a cluster.

### Changed

- **The smaller template disks are confirmed by one lab build each** (`docs/template-build-disk.md`): a
  native `ubuntu-2604` template at 5 GiB and a Kubernetes 1.37.2 node template at 8 GiB were built on
  the lab and exported through the export workflow; both images are listed in
  `docs/vm-artifact-publishing.md`. The native 5 GiB size is close to its limit on Ubuntu 26.04: the
  root filesystem is 3.7 GiB and the build left less than 1 GiB free before its cleanup. Not done: the
  other native OS slugs at 5 GiB, a portal import of either image, a node group joining a cluster.
- **Native Ubuntu 26.04 templates build at 6 GiB** (`BUILD_DISK_GIB_BY_OS` in `images/build.py`; 6 GiB
  added to the sanitizer's allowlist). Its vendor image has a separate `/boot`, and the 5 GiB build kept
  less than 1 GiB free on `/` after the upgrade, too close to the 1 GiB admission of the cleanup step.
  Ubuntu 24.04 at 5 GiB (built and exported on 2026-11-15, 1.7 GiB free) and the other OS keep 5 GiB.
  Templates and images already built at 5 GiB stay valid. The first 6 GiB lab build (2026-11-15) kept
  2.0 GiB free after the upgrade and was exported through CI; the image is listed in
  `docs/vm-artifact-publishing.md`.
- **A Proxmox provider's `config.network` is checked when it is saved** (`POST /api/v1/admin/service`,
  `PUT /api/v1/admin/service/{id}` and `PUT /api/v1/admin/service/{id}/update`): the block was stored as
  sent, so a `cidrPool` of `/25`, a zone id of nine characters or a VLAN zone without a bridge showed up
  only as a failed or 503 network create in a tenant's project. A save whose body carries `network` is now a
  `400` naming the field when `cidrPool` is not an IPv4 range of `/24` or larger, `defaultZone` names no
  zone of `zones` (or there are several zones and no `defaultZone`), a zone's `kind` is not `evpn`, `vlan`
  or `simple`, a zone id is longer than 8 characters, a `vlan` zone has no `bridge`, a `vlan` zone's
  `vlanMin` or `vlanMax` is missing or not a whole number from 1 to 4094 (a VLAN tag), or `vlanMin` is
  above `vlanMax`, or `isolation` is `evpn-per-project` with a default zone that is not EVPN. What is only missing is still accepted (no
  `cidrPool`, no zone yet, as the console's Add provider form sends it). A save is refused only for a
  problem the block it replaces does not have already: the console's Connection tab sends the stored block
  back with every save, and a block saved before this check may work (a zone that exists in Proxmox is used
  as it is, and the portal reads only the default zone), so it must not lock the provider. A new problem, or
  a wrong value changed to another wrong value, is still refused. The dev
  seed and the network block installer stage 60 builds from the inventory defaults pass. Installer stage 00
  now refuses, before anything is changed, an inventory the check would refuse at stage 60 after stages 10 to
  55 had run: `network.kind` not `evpn`, `simple` or `vlan`, `kind: vlan` with the shipped empty `bridge`
  or with a `vlan_min` or `vlan_max` that is not a whole number from 1 to 4094 or a `vlan_min` above
  `vlan_max`, `isolation: evpn` with `kind: simple` or `vlan`, a `cidr_pool` that is not IPv4 `/24` or larger (or is
  empty), and a `default_zone` that is empty or longer than 8 characters. A site whose VLAN zone was made by
  hand in Proxmox with no bridge in the inventory sets `network.bridge` to that zone's bridge (the portal
  uses an existing zone as it is). Tested with unit tests and the preflight task run on its own against the
  inventory defaults; not run in a full install.

### Fixed

- **The Proxmox provider page saves the failover URLs and the cluster CA** (operator console, Cloud providers,
  Connection tab). The form showed both fields, but Save sent neither, so an edit of `apiUrls` or `caCert` was
  dropped while the page reported it saved. Save now takes the connection keys from the same mapping as Add
  provider. A Playwright spec checks the request body against the mock API; not run against a live API.
- **The admin console's Proxmox resources editors have a label a screen reader announces**
  (`CloudProviderDetailPage.tsx`): the five JSON text areas (network, storage profiles, OS templates, GPU
  mappings, server seeds) were not tied to their visible headings, so assistive technology read them as
  unlabelled edit fields. Each heading is now the text area's label. Checked with the axe matrix of
  `e2e/a11y.spec.ts`, light and dark.

### Security

- **HIGH/CRITICAL findings without a fixed version stay tracked until 2027-03-12** instead of
  2026-12-12 (`UNFIXED_REVIEW_BY` in `.github/scripts/check_scan_report.py`; owner decision of
  2026-11-15). The reason is the review in `docs/security-unfixed-review.md`: per-target counts by
  Debian status for the eleven non-GPU templates, the controlled-base prototype and the VM export tool
  image (the product images and lockfiles have none), which findings a third party's data can reach,
  and what was not checked. The gate still blocks every finding with a fixed version in those targets,
  and from 2027-03-12 the unfixed ones again. The default `python` template is outside that set: it is
  a public image run as is and scanned report-only, so its 2 HIGH zlib findings with a fixed version
  and its 47 without one do not block, before or after 2027-03-12. The single VEX (CVE-2025-47273) did not move: it stops applying
  on 2026-12-31 (UTC), and from then the setuptools finding it covers blocks in nine templates and the
  prototype unless the owner decides otherwise.
- **The job triggers on the management port are no longer exposed when the scheduler is on**
  (`cmd/api/main.go`, `debugTriggers`). The API mounted every unauthenticated `POST /debug/*` trigger
  on the management port (`:8081`, every interface) when `PROXCLOUD_JOBS_SCHEDULER_ENABLED` **or**
  `PROXCLOUD_JOBS_DEBUG_TRIGGERS` was true. That includes `run-charge`, `run-collect`,
  `run-send-bills`, `run-dunning`, `run-project-deletion` (deletes cloud resources),
  `run-registry-prune`, `send-test-mail` (mail to any address), `sse-emit` and `jobs-status`; only
  `gen-hmac-key` was already limited to debug triggers. The chart never routes the port through its
  gateway or ingress, but any pod that could reach the API pod, or any process on a Docker host
  through the container's bridge address, could call them. This was in the code since the first
  ProxCloud release and is in every tag from `v0.4.0-rc.1` to `v0.6.0`. Affected: every install with
  the scheduler on (Helm `api.jobs.schedulerEnabled: true`, installer `jobs.scheduler_enabled: true`,
  or the environment variable), which is every production install that bills on a timer. Now only
  `PROXCLOUD_JOBS_DEBUG_TRIGGERS` mounts them; health and readiness on the port are unchanged.
  **Behaviour change:** a script that called a `/debug/*` trigger on an install with only the
  scheduler on now gets `404`. Migration: on a production install, use the authenticated
  `/api/v1/admin/job/*` endpoints (admin credential), which cover charge, metrics, sync, collect,
  savings expiry, reminders and the transaction scan, and `GET /api/v1/admin/durable-jobs` for the
  `jobs-status` history; the mapping is in `docs/jobs-scheduling.md` ("Manual triggers"). The other
  triggers have no admin endpoint: set `PROXCLOUD_JOBS_DEBUG_TRIGGERS=true` (Helm
  `api.jobs.debugTriggers`, installer `jobs.debug_triggers`) only on a development or lab install to
  keep them. Docs, the chart's values and README, the installer defaults and the development compose
  file say so. Verified by unit tests only (the management router with the scheduler on and triggers off answers `404` on
  `/debug/*`, the admin job endpoints still run their jobs); not checked on a running install.

## [0.6.0] - 2026-11-14

A feature release on top of `v0.5.2`, with no breaking API change.

- The portal can be installed on one host with one command (`deploy/compose`).
- VM images are now exported, published, attested and verified by one GitHub Actions run; nothing is
  prepared by hand. The images published this way are listed in
  [docs/vm-artifact-publishing.md](docs/vm-artifact-publishing.md).
- Template disks shrink to 5 GiB (native OS) and 8 GiB (Kubernetes node). **Upgrade portals before using
  images built with the new sizes**: portals up to `v0.5.2` refuse an image below 20 GiB.

Everything `v0.5.0` to `v0.5.2` say is **not** verified still holds. Also not verified: a lab build with
the new disk sizes, Let's Encrypt issuance in the Compose install, and an import of a CI-published image
into a portal.

### Added

- **One workflow run from a template to a published VM image** (`vm-artifact-export.yml`): choose the
  variant, the OS, the Kubernetes version and the template VMID, and one manual run on `main` does the rest.
  The `export` job (self-hosted, inside the lab, no registry credential) guards and copies the template
  read-only, then runs inventory, review, sanitize, postscan and the two-clone cold boot in a fixed order
  (`images/vm_export/pipeline.py`: a stage refuses unless the earlier ones completed and their records
  match), and builds the three-file bundle. Hosted jobs then publish it to GHCR (only for the variant,
  OS and Kubernetes version that were dispatched), create a GitHub build-provenance attestation for the
  digest of that push, check it with the command the run prints, and pull the digest with no credential
  to verify the files. The run also uploads one public evidence record (hashes, check results, versions,
  the tool image digest, the commit and the run), built from an allowlist so it holds no path, address,
  VMID or guest data; its sha256 is attested too and the final summary prints it. A cold boot that could
  not clean up keeps its state on the runner, and the next export finishes that cleanup before it
  starts. `docs/vm-artifact-publishing.md` has the flow, the trust model with the settings the owner
  must make outside the repository (runner group, fork approval, branch protection, the environment
  `vm-artifact-export`), the secrets and variables to set (`EXPORT_TEST_ADDRESSES` and `EXPORT_VLAN` are
  new) and the commands anyone can use to verify an image. The workflow has run on the lab for
  `ubuntu-2404` and for the three Kubernetes versions; the document lists those images.

- **VM export tool image built by CI** (`images/vm_export/tools/`, `vm-export-tools.yml`): the image the
  export containers run from (`qemu-img`, Python with libguestfs, the appliance kernel) was built on a
  workstation and loaded onto the runner by hand. It now has a recipe in the repository: Debian 12 pinned
  by digest, every package from one `snapshot.debian.org` timestamp and checked against a committed
  package list, no `sudo`, no account and no setuid file, usable as any non-root uid. The workflow builds it on a hosted runner, runs a smoke test without network as an
  arbitrary uid, scans it with the existing Trivy gate and, on `main`, pushes it to
  `ghcr.io/mutex-solutions/proxcloud-vm-export-tools` under one tag that the workflow never replaces. The
  digest is recorded in the job summary and as an artifact. `capsule.py` now pins that published image by
  registry digest: the export job pulls it and containers start only from its local ID.

- **VM image export steps as repository code** (`images/vm_export/`): the copy, inventory, review,
  sanitize, postscan and bundle steps that prepared the published VM images were private operator
  scripts. They are now reviewed modules with one evidence chain: a guarded read-only copy receipt, an
  inventory bound to that copy, a review derived from a committed baseline
  (`baselines/ubuntu-2604-k8s.json`, only the Kubernetes package versions may differ), a sealed
  sanitize run and a postscan bound to it. Each step runs in a container with no network, no
  capabilities and read-only inputs, and the parent checks the container's settings before start and
  proves it stopped. The container runs as the invoking host user (non-root, uid and gid 1000 or
  higher), no longer only uid 1000, so a CI runner account can run it. The cold-boot acceptance is
  code too (`vm_export.coldboot`): through the PVE API with the build token it uploads the sealed
  disk, boots two clones with fresh cloud-init, checks both guests over SSH (a failed snapd change
  fails the check), requires snapd on both and different machine-id, host key and snapd identity, and
  removes only what its own state file names. Names, the ownership tag and the upload carry a random
  per-run token, so two runs of one disk never touch each other's VMs or files. Cleanup ignores a
  cancel, retries, and can be repeated from the state file. The export workflow above runs them.

- **Production install on one host with Docker Compose** (`deploy/compose/`): `./proxcloud-up.sh` asks for the
  domain (or four hostnames), the TLS mode and two email addresses, generates every secret once into `.env`
  (mode 600), starts Traefik, Keycloak in production mode, PostgreSQL, the API and both consoles at the pinned
  release, creates the first operator with a temporary password and prints the URLs. TLS modes: `acme-http`
  (Let's Encrypt HTTP-01), `acme-dns-cloudflare` (DNS-01, the token as a Docker secret file) and `internal`
  (Traefik's self-signed certificate, for a LAN evaluation). Also `upgrade <version>` (backup first, pull before
  anything changes), `status`, `backup` and `uninstall` (volumes are deleted only with `--purge`). The Compose
  project is `proxcloud-prod` by default, and the install refuses a project name that the development stack,
  another directory's containers or a database volume without its `.env` already use. `--dry-run` writes its
  preview to `.env.dry-run` and never changes `.env`; variables of the calling shell do not reach Compose. No
  password is a command argument, inside the containers either. The Keycloak
  realm settings and the first-run seed are rendered from the Helm chart's own templates and a test keeps them
  equal; the default image tag is part of the release contract test. It installs the portal only: no Proxmox
  provider, no managed services. Guide: `deploy/compose/README.md`, quick start path F.
  **Verified** on one Docker host with `internal` TLS: install, re-run, hostname change, backup and restore into
  an empty database, upgrade to the same release, uninstall with and without `--purge` (and the install after a
  purge, which creates the operator again), a switch of the TLS mode and back, a realm import that fails during
  that switch (the running portal, its Traefik included, is left untouched), and the refusal of a project name
  in use. In the two Let's Encrypt modes Traefik was only started against an unreachable CA: the configuration
  loads and requests a certificate for the four hostnames. **Not verified**: certificate issuance and renewal
  (HTTP-01, and DNS-01 through Cloudflare), and an upgrade between two different releases.

### Changed

- **The hand-fed VM publishing path is removed**: `vm-artifact-stage.yml` read a directory an operator had
  filled on the runner, and `vm-artifacts.yml` took a run id and two hashes typed by hand. Both are gone.
  `vm-artifacts.yml` is now a reusable workflow that only `vm-artifact-export.yml` calls, and it accepts
  one producer: the successful `export` job of the same run, at the same commit on `main`
  (`images/vm_publish_stage.py`). `images/vm_stage_local.py` is deleted. Images published earlier stay
  where they are; an export through the workflow gets a new tag. The runner label
  `proxcloud-vm-artifact-stage` is no longer used.

- **Smaller template disks: 5 GiB for native OS templates, 8 GiB for Kubernetes node templates** (both were
  20 GiB). The built images hold 0.4 to 1.3 GB (native OS) and about 4 GB (Kubernetes node), and every clone,
  copy and sanitize run paid for the whole 20 GiB. A server or node made from a template still gets the root
  disk size its user asks for: cloud-init grows the filesystem at first boot. One constant per builder
  (`DEFAULT_BUILD_DISK_GIB` in `images/build.py`, `K8S_BUILD_DISK_GIB` in `images/k8s-build-config.py`).
  - **A server with a root disk below 20 GiB now gets that size.** With a 20 GiB template a smaller request
    (the seeded `s1-small` flavor has 10 GiB) still produced a 20 GiB disk and counted 20 GiB against the
    quota; with a 5 GiB template it produces and counts 10 GiB. The create page's minimum root size drops
    from 20 to 5 GiB.
  - The minimum disk of an image is the largest of its per-node template copies, not the newest copy's. A
    cluster where one node has the 5 GiB copy and another still has the 20 GiB copy keeps a 20 GiB minimum:
    before, a 10 GiB server placed on the second node passed the check, counted 10 GiB and got 20 GiB.
  - The sanitizer and the export tools accept an explicit allowlist per variant instead of one fixed size:
    native OS 5 GiB or 20 GiB, Kubernetes node 8 GiB or 20 GiB. Any other size is refused, the export must
    keep exactly its source's size, and the facts and the manifest carry the actual size. The cold-boot
    acceptance requires the import to have the artifact's own size (reason `import_size_mismatch`, formerly
    `import_not_20g`), then still grows it to 30 GiB and checks the growth in the guest. The Kubernetes
    review checks the source size against the allowlist, not against the 20 GiB recorded in the committed
    baseline.
  - The artifact contract's minimum virtual size is 5 GiB (was 20 GiB) in `images/vm_artifact.py` and in the
    portal's Template updates import.
  - **Compatibility:** portals up to v0.5.2 refuse an image whose virtual disk is below 20 GiB (invalid
    artifact contract, nothing is downloaded), so an image built with the new sizes needs a portal with this
    change. Existing 20 GiB templates and the already published 20 GiB images stay valid; nothing is rebuilt
    or resized. **Upgrade the portals before the third small image of a line is published:** a portal up to
    v0.5.2 checks only the newest three tags of a line, so after the third 5 or 8 GiB image it offers nothing
    on that line, and the older 20 GiB image is importable only by an API call that names its digest.
  - A build that runs out of disk space still fails: the provisioning scripts stop at the first failing
    command, and the last cleanup step of both builders now refuses a root filesystem with less than 1 GiB
    free (in both network modes of the Kubernetes build). The new sizes were not yet confirmed by a lab build; what the first build of each image type has
    to confirm is listed in [docs/template-build-disk.md](docs/template-build-disk.md).

- **Helm chart: the realms derived by `keycloakConfigCli` have Keycloak's brute force detection on**
  (`bruteForceProtected`, Keycloak's default thresholds: a temporary lockout after repeated failed logins). Their
  token endpoints are public and the console clients allow the password grant. Applied at the next
  `helm upgrade`, and by the Compose install, which renders the same realm files.

### Fixed

- **Native OS images kept the build VM's snapd identity**: the offline sanitizer reset the snapd device
  key and serial only on the Kubernetes path. The native Ubuntu path now does the same reset and its
  final check refuses a remaining key, serial, session macaroon or store user.

- **Kubernetes 1.37 node templates did not build**: the pinned image-builder `v0.1.55` renders its kubeadm
  config with `kubeadm.k8s.io/v1beta3`, which kubeadm 1.37 refuses ("old API spec"), so
  `kubeadm config images pull` failed the build. The build now applies upstream's own fix (image-builder
  `db2e0f41`, after `v0.1.55`) to the exported pinned tree: `v1beta4`, read by kubeadm since 1.31, for every
  selectable version. The pinned file's hash is checked first; any other content refuses the build.
  Found on the first 1.37.2 lab build.

## [0.5.2] - 2026-11-14

Patch release on top of `v0.5.1`, with no breaking change. It fixes VM template smoke-test and import
cleanup on PVE 9, where the least-privilege token gets 403 instead of 404 for a VM outside its pool
(found on the first real import + smoke run, a homelab running PVE 9.2.21). Everything `v0.5.0` and
`v0.5.1` say is **not** verified still holds.

### Fixed

- **A VM template smoke test that passed ended `dead` in `cleaning` on PVE 9**: the provider token reaches a VM
  only through its pool, so after the smoke job destroyed its test VM, PVE answered the follow-up config read with
  `403 Permission check failed (/vms/<id>, VM.Audit)` instead of "does not exist", and the job retried the
  already-done cleanup until its attempts ran out (the result, PASS, was kept; the dead-job pass would have
  retried every 10 minutes). The import and smoke jobs now treat a 403 for a VMID that is not (or no longer) a
  member of `pc-templates` (on any node) as out of view: gone, or never theirs, and never touched. A 403 for a
  member, or with a pool or task list that cannot be read, is still an error. PVE adds a created or cloned VM to
  its pool only when the task ends, so the import job also keeps the error while its VMID's create task runs, and
  the smoke job while a clone of the template runs; the task is checked before the pool, so a task that ends in
  between has already joined it. A clone not yet started when the check runs (a superseded attempt) is left to the
  orphan sweep.
  Found on the first real import + smoke run (PVE 9.2.21).

## [0.5.1] - 2026-11-14

Patch release on top of `v0.5.0`, with no breaking change. It fixes the VM template import, which
failed on PVE 9 with the documented least-privilege token (found on the first real import, a homelab
running PVE 9.2.21), and lets the image publication bridge accept Kubernetes node templates 1.36.6 and
1.37.2 next to 1.35.10. Everything `v0.5.0` says is **not** verified still holds: both template-update
flags default **off**; the VM import and its smoke test are still not accepted on PVE 9.2 (the fix was
reproduced and verified against one real PVE 9.2.21 cluster, the full import + smoke run follows the
release); the sandbox mirror is not lab-verified; and live installer (P6) and split/managed-service
(P7) acceptance were not performed. The security gate policy is unchanged.

### Changed

- **Public VM artifacts: Kubernetes node templates 1.36.6 and 1.37.2** are accepted for publication next to
  1.35.10 (Ubuntu 26.04 only), so clusters can be tested on an upgrade path 1.35 → 1.36 → 1.37. The publisher,
  the offline sanitizer and the copy policy share one explicit allowlist (`K8S_VERSIONS`), kept equal by a test;
  the copy policy now requires `kubeadm`, `kubelet` and `kubectl` at exactly the reviewed version. Any other
  version is still refused. Each new version still needs its own build, inventory and runtime acceptance.

### Fixed

- **VM template import failed on PVE 9 with the least-privilege token**: the import created its VM with
  its ownership tag in the create call, and PVE 9 checks a create's tags with `VM.Config.Options` on
  `/vms/<id>` without the pool, which the provider token never holds (its rights reach a VM only through
  `pc-templates`). The create task failed `403 Permission check failed (/vms/<id>, VM.Config.Options)` and
  the job retried until it gave up. The VM is now created untagged and tagged by the import write; until
  then it is the job's only by its exact build name, membership of `pc-templates` and the absence of every
  tag, and the orphan sweep recognises such a VM too. Found on the first real import (PVE 9.2.21).

## [0.5.0] - 2026-11-13

Second stable ProxCloud release, after `v0.4.0` (owner decision 2026-11-13: this is
`v0.5.0`, not `0.4.1`). It adds portal template updates from GitHub Packages with a
GitHub classic PAT integration and a staged → smoke test → publish/withdraw
lifecycle, sandbox template image updates mirrored from GHCR into the sandbox
registry (off by default), a per-provider attach policy and bulk attach of a provider
to existing projects, a real month-end cost forecast, and Kubernetes node-image
sanitizer fixes for the real template. It has **breaking API and CLI changes**, listed first below;
review them before upgrading scripts, clients or installer inventories.

What is **not** verified: `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED` defaults **off**,
and the VM import and its smoke test have **not been lab-verified on PVE 9.2**; the
GitHub classic PAT integration has **not been verified against the real GitHub
packages**. Sandbox image mirroring from the portal is included but
`PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED` defaults **off**, and it has **not been
lab-verified** on a real sandbox cluster with Zot and GHCR (copy, digest check, prepull
rollout, adoption, the stage-55 admission-policy probe). It pulls only the **public**
`sandbox-*` packages, with an anonymous pull token; the GitHub PAT never reaches the
sandbox cluster. With the flag off, sandbox template updates stay on the manual
`deploy/templates/images.yaml` pin path. **Upgrade note**: the first `make upgrade`
with this release restarts the sandbox registry (Zot) pod once (its users Secret gains
the `mirror` account); pulls fail for the restart's few seconds.
As for `v0.4.0`, "stable" does not mean full acceptance: live installer (P6) and
split/managed-service (P7) acceptance were **not** performed, the job scheduler and
debug triggers stay off by default, and production mail delivery and the
managed-sandbox saved lifecycle remain held. The security gate policy is unchanged
from `v0.4.0`: HIGH/CRITICAL findings without an upstream fix are tracked, not
blocking, until 2026-12-12, and the single VEX expires on 2026-12-31. Upgrading a broker-era installation still needs the `v0.4.0`
drain/reconcile and retention runbook. The `v0.5.0` tag publishes the images and app
chart `0.5.0`; verify that they can be pulled before installing with
`images.build: false`. The managed charts are unchanged (`database-cluster` `0.5.1`,
`proxmox-kamaji-cluster` `0.1.10`).

### Breaking changes

- **Attaching a provider to a project is `POST` (#214).** The admin attach is
  `POST /api/v1/admin/project/{id}/external-service/{externalServiceId}`. `GET` and
  `HEAD` on that path answer `405` and change nothing; the old `GET` provisioned the
  project on the provider. The attach stays idempotent: re-attaching a service the
  project already carries makes no provider call and adds no second binding. Scripts
  calling the old `GET` must switch to `POST`.
- **A named service the project is not attached to is refused (#215).** A project
  cloud call that names a service (`x-service-id` or `?serviceId=`) the project is not
  attached to now answers `400 Project is not attached to service <id>` (`…to the
  requested service` when the id is not a plain service id) before any provider is
  called, instead of silently running on the project's Proxmox service (a BUCKET
  create for a newly registered S3 store failed on Proxmox). This covers creates,
  actions, deletes and the reads `quota-usage`, `gpu-capacity`, `public-networks` and
  the S3 key routes. Requests naming no service keep the Proxmox default (else the
  project's first attached service). Existing projects still need an operator to
  attach a provider registered after they were created (per project, or with the bulk
  attach below).
- **`POST /api/v1/project/{id}/init` can answer `409` (#216).** It no longer answers
  `200 {}` when it leaves the project down. It answers `409` with `errors.details`
  `{status, reason[, billingProfileStatus]}`, reason one of
  `billing_profile_inactive`, `billing_profile_suspended`, `billing_profile_missing`,
  `disabled_by_operator`, `scheduled_for_deletion`, `deletion_in_progress`, `deleted`,
  `not_enabled`; `billingProfileStatus` is present when a paying billing profile
  exists. Success is unchanged. The customer API error envelope may now carry an
  optional `details` object; clients must ignore details they do not know. The client
  console shows the reason above the project's pages, with a link to Billing where
  that is the fix.
- **VOLUME create answers the created volume (#218, shipped in #220).**
  `POST /api/v1/project/{id}/cloud` with `{"type":"VOLUME"}` answers the volume's cache
  row: `id`, `externalId` (the PVE volid) and `data.volume` (`name`, `size`,
  `status`, …), so the caller can use the volume at once. It used to answer an empty
  `result` (create on a server) or `{"result":{"node":…}}` (standalone create); the
  standalone create **no longer returns `result.node`** (the node is in
  `data.volume.availability_zone`). If the new row cannot be read back yet, the answer
  is the volume as the create knows it, without an `id`.
- **Publishing at import is removed (#223).** `POST /api/v1/admin/template-updates/vm`
  with `"publish": true` is refused with `400` (`errors.details.code =
  publish_at_import_removed`) before any provider or GitHub call; an absent or `false`
  `publish` is ignored. Every import lands **staged**, tagged
  `pc-tu-import;pc-tu-staged` without any catalog tag, until it passes its smoke test
  and is published.
- **`images/build.py --keep` must be 1–999 (#223).** `--keep 0`, which used to prune
  nothing, is now refused with a clear message; the installer's preflight checks
  `proxcloud.templates.keep` the same way (default 2, unchanged).
- **The per-project admin attach refuses projects being deleted (#222).** A project
  scheduled for deletion, being torn down or deleted gets
  `400 Project is being deleted. Cannot attach a provider` instead of being
  provisioned.

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
- Sandbox template images can be updated from **Template updates** (`docs/template-updates.md` §5,
  off until `PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true`): **Update** mirrors the offered
  `ghcr.io/mutex-solutions/sandbox-<name>@<digest>` (re-verified) into the sandbox registry as
  `library/sandbox-<name>:<tag>` with the same digest — a fenced, resumable durable job running the
  trusted `sandbox-import` worker in a new copy mode with pod-owned credentials, the destination digest
  checked by the portal — then points the prepull DaemonSets at it; the existing adoption job switches
  templates once every Ready node has pulled it. **Rollback** points the prepull back at the previous
  pin. Same-digest updates are refused, one job per image at a time, audited
  (`admin:service:manage`). Installer: stage 55 creates a Zot `mirror` account (read/create on
  `library/sandbox-*` only), a ValidatingAdmissionPolicy limiting the portal to init-container image
  changes to mirrored digest refs of the same repository and, only once a dry-run probe shows that
  policy enforced, a Role (get/patch on the two prepull DaemonSets); stage 60 hands the account over as the write-only `secret.registryMirror`. Each switch is
  recorded as a `sandboxBuiltinPin`; `make upgrade` reads the pins through the admin API and renders
  the newer of `images.yaml` and the pin, printing the line to commit, and stops (instead of rolling
  back) when it cannot read them; `DELETE /api/v1/admin/template-updates/sandbox/pins/{id}` lets
  `images.yaml` win again. The worker gets only an anonymous short-lived GHCR token pull-scoped to the one public package, checked never to be or contain the GitHub PAT (used for discovery only; private packages are refused, `ghcr_package_not_public`). The admission policy compares fields by name, so stage
  55 first checks the sandbox cluster's OpenAPI schemas and stops the upgrade on any field the policy
  does not cover; prepull init containers now run non-root with a read-only root and no capabilities.
  **Upgrade note**: the first `make upgrade` with this change restarts the sandbox registry (Zot) pod
  once, because its users Secret gains the `mirror` account; pulls fail for the restart's few seconds.
  Residual risk: the `mirror` account may create a new `library/sandbox-<name>` repository (it cannot
  overwrite or delete an image). With the flag turned off, a sandbox job that is queued, or whose
  runner is gone (expired lease), ends `failed` `feature_disabled` (or keeps a failure code already
  recorded; one already pinned is acknowledged as succeeded; one whose lease expired on its last attempt
  ends `job_dead` through the dead-job pass) without running further: its worker pods
  and their Secrets are deleted and a pin whose prepull switch never landed is restored, so it no
  longer blocks its image or the pin delete; a job another replica (flag on) holds is not touched.
  A failed job keeps its image busy (a new update or a pin delete answers 409) until its cleanup
  has run; that cleanup (the job's worker pods, a partial prepull switch, the pin) needs only the
  provider's cluster, so it also runs when the provider lost its mirror account or moved its
  template registry.
- Add a per-provider **attach policy** for new projects: `config.attachToNewProjects`
  (default `true` when absent, so every existing provider keeps attaching new projects). With
  it off, a new project's first bootstrap skips that provider. Set it on the provider's new
  **Projects** tab or with `PUT /api/v1/admin/service/{id}/attach-policy`, a single-key write
  that changes nothing else and does not race the connection save. A non-boolean value is
  refused there and on the provider create/connection save.
- Add **Attach to existing projects** (follow-up of #215): from a provider's Projects tab,
  attach it to all projects, the projects of picked organizations, or picked projects. It runs
  as a fenced, resumable PostgreSQL durable job (`providerattach.run/v1`), at most one per
  provider, with 1–4 projects at a time and at most 5000 projects to attach per run (planned
  in resumable batches), and goes through the same path as the per-project attach. Already
  attached projects are skipped with no provider call; projects being deleted are skipped;
  suspended projects are attached and stay suspended. The provider is re-read per project: one
  disabled or deleted mid-run ends the run and the rest can be retried. Results per project use
  fixed codes only and are kept 30 days. The tab shows progress, the failed projects and their
  reason, and offers cancel and retry-failed. Start, cancel and retry are audited
  (`BULK_ATTACH`, with actor, provider, scope and counts). Start and retry need
  `admin:service:manage` and `admin:project:update`, cancel `admin:service:manage`; reading
  needs `admin:service:read` (and `admin:project:read` for the project picker and preview).
  API: `/api/v1/admin/service/{id}/bulk-attach…` (`docs/cloud-integration.md`).
- Add the **template lifecycle** to Template updates (`docs/template-updates.md` §4): every import lands
  *staged* (hidden); an automatic, durable **smoke test** boots a throwaway clone in pool
  `pc-templates` on the provider's smoke network (`config.templateSmoke`), with fresh cloud-init and an
  ephemeral SSH key, checks the guest agent, `cloud-init status`, root-disk growth (and kubelet +
  containerd for Kubernetes node images) and always destroys the clone; **Publish** (only after a PASS
  of exactly that template and digest) makes the newest version the default for new VMs and keeps older
  ones; **Withdraw** (reason required) takes a bad version out of the catalog — falling back to the
  previous one — keeps its template, and blocklists its digest for import, smoke and publish. Withdrawing
  the last published version of an OS needs an explicit forced confirmation. A withdraw covers every
  provider holding a copy of the digest; a copy that could not be changed yet (provider unreachable)
  shows *Withdraw incomplete* and the same action finishes it. A publish requires the template to be
  unchanged since its passing smoke test. All actions need
  `admin:service:manage`, are audited, and stay behind `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED`. The
  admin page gains an *Imported templates* tab with the state of each template and its actions.
- The first public Kubernetes node VM image,
  `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s` (tag
  `ubuntu-2604-k8s-1.35.10-20261112032843-8a2a737b331b`), was published on 2026-11-12
  through the manually dispatched VM artifact publisher. It is a separate QCOW2 artifact for
  reviewed hypervisor import: it is not part of the app chart, the installer BOM or the
  `v0.5.0` tag. No kubeadm join or NodeReady is claimed for it; node acceptance stays
  the runtime checks in `docs/public-vm-sanitization.md`.

### Changed

- `images/build.py build` never prunes a template the portal imported (marker `pc-tu-import`, or a
  `pc-src-` tag from an earlier import), nor the newest published version of an OS on a node.
- `pve.DiscoverTemplates` skips templates tagged `pc-tu-withdrawn`, even when `pc-public` is present.

### Fixed

- The per-project admin attach runs for at most 2 minutes (its refusal of a project
  being deleted is listed under Breaking changes). A project's bindings are now only
  appended atomically: a project save (rename, member change, activation) no longer
  rewrites `services` or `provisionFailures` from a stale read,
  so it cannot drop a provider binding added meanwhile. Operator attaches and the first
  bootstrap of one project are serialized under a per-project lock; the binding write runs
  on the lock's own session with the ownership check in the same statement, so a writer
  that lost the lock writes nothing. Attaching never stores a copy of a Proxmox provider's
  default quota on the project: as before, a project without its own quota follows the
  provider default live. An operator attach of a project never opened marks its binding in the same write, so
  the project's first open still provisions every provider whose policy is on.
- The month-end forecast is now a real projection instead of always equal to the
  current month's cost (#217). `forecastedMonthEndCosts` in `GET /project/{id}/cost-info`,
  `GET /bill/{bp}/cost-info` and `GET /admin/billing-profile/{id}/cost-info` is the
  current cost plus what the charge job will still add before the UTC month end for the
  resources it bills now, rated read-only with the same engine and rules. Tax,
  savings-contract discounts, price-adjustment rules and future price or resource
  changes are not modeled. `forecastApproximate`, `forecastApproximateReasons`
  (`unrated`, `usage`, `stale`, `unavailable`, `external_billing`) and
  `forecastUnratedTypes` say when part of it is held at its current cost. A resolution
  is cached 60 s per profile and month and bounded to 3 s; a timeout, error or a profile
  with more than 2000 billable resources gives `unavailable` (forecast = current), never
  a failed read. The admin profile list keeps forecast = current, flagged approximate.
  The consoles label the figure as an estimate and explain why it is approximate
  (`docs/billing.md`).
- The client console's server field helpers moved out of the Servers page module, so
  the page hot-reloads with React Fast Refresh during development (#218).
- Kubernetes node-image sanitizer (`--k8s-os ubuntu-2604`) fixes for the real Ubuntu
  26.04 / Kubernetes 1.35.10 template, which the previous code refused (fail closed) on
  a read-only inventory (#219):
  - only the exact canonical `/dev/disk/by-uuid/<uuid>` root fstab spec is resolved,
    and it must be the inspected root; dpkg `hold ok installed` is accepted for the
    pinned Kubernetes packages; only active swap rows count; only an empty
    `.kubelet-keep` placeholder is ignored in the static manifests;
  - machine identity is reset in the copy (owner decisions): `builder` leaves every
    supplementary group and must own an unshared primary group, the sudo policy is
    bound to reviewed hashes (any `/etc/sudoers-rs`, active `/etc/sudo.conf` line or
    userdb record refuses), snapd is reset to unregistered (a session macaroon or a
    store user refuses), and the cloud-init default user is the upstream `ubuntu`;
  - the inventory mounts the root read-only, and the root is remounted read-write only
    after the policy accepts.

  The template apt-holds `cloud-init`, `openssh-server`, `qemu-guest-agent`, `snapd`
  and `kubernetes-cni` besides the Kubernetes packages; consumers must
  `apt-mark unhold` them to receive security updates. Runtime acceptance (cold boot,
  two-clone snapd re-registration) is not claimed by this source change.

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
