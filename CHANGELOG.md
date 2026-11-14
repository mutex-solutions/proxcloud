# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
  new) and the commands anyone can use to verify an image. The workflow has not run on the lab yet.

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
