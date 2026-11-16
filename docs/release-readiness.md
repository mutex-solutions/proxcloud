# Current support boundaries and first-release readiness

Status snapshot: **2026-11-15**, preparing the stable release `v0.7.0` on top of `v0.6.0`
(tag 2026-11-14). This is an evidence summary, not a release announcement or a production SLA.
`v0.4.0-rc.1` (tag 2026-11-11) was published as a non-latest prerelease, `v0.4.0` (tag 2026-11-12)
as the first stable release, `v0.5.0` (tag 2026-11-13) as the second, the patches `v0.5.1` and `v0.5.2` and the
stable `v0.6.0` (tags 2026-11-14), and `v0.7.0-rc.1` (tag 2026-11-15) as a non-latest prerelease. Pushing the `v0.7.0` tag
publishes the stable images, app chart and GitHub Release. `v0.5.1` to `v0.7.0` have no breaking API change against
`v0.5.0` (`v0.6.0` changes the VM image size contract; `v0.7.0` stops serving the management port job triggers with
only the scheduler on and refuses a new problem in a Proxmox provider's network block on save; see the changelog);
`v0.5.0` contains breaking API changes against `v0.4.0`; see the [changelog](../CHANGELOG.md). This source change publishes
nothing and performs no live acceptance; the [lab acceptance of Platform setup](#platform-setup-lab-acceptance)
was run on `v0.7.0-rc.1` before it. Check GitHub's Releases/Actions pages before choosing a deployment
commit.

## What is available now

- Source on `main`, public development container images on GHCR, the
  `v0.4.0-rc.1` and `v0.7.0-rc.1` prereleases and the stable releases `v0.4.0`, `v0.5.0`, `v0.5.1`, `v0.5.2` and
  `v0.6.0` (images and app charts `0.4.0-rc.1`, `0.7.0-rc.1`, `0.4.0`, `0.5.0`, `0.5.1`, `0.5.2` and `0.6.0`). The
  app chart `0.7.0`, appVersion and installer BOM `v0.7.0` name the current stable release; its tag publishes those artifacts.
  Verify they are pullable before using `images.build: false`; a source pin is not proof that a registry artifact exists.
- The installer defaults to `images.build: true`, building the API and two
  consoles plus app chart from the checkout. A source/local-registry installation
  is a different path from pulling a released chart with `images.build: false`.
- Five images have a CI publication path: API, customer console, admin console,
  sandbox proxy and sandbox runtime. The local-image installer stage builds only
  the first three; sandbox images require separately available, compatible pins.
- The installer minimum is **PVE 9.2**, with the documented network, firewall and
  storage prerequisites. Controlled OS-template canaries ran on a PVE 9.2 host;
  there is no general compatibility or production certification for every later
  PVE version, storage backend or cluster topology.

See [quickstart](quickstart.md), [installation](install.md),
[CI/release operations](ci-release.md) and the
[chart guide](../deploy/charts/proxcloud/README.md) for executable instructions.

## Evidence is narrower than feature availability

| Area | Evidence / boundary |
|---|---|
| Backend and consoles | Hosted Go unit/integration tests, both SPA smoke builds, installer tests and image builds pass on reviewed commits. A green build is not an end-to-end user journey or security certification. |
| Debian 13 OS template | [Controlled build/smoke canary](https://github.com/mutex-solutions/proxcloud/actions/runs/42730711792) succeeded. Final-template readback verified cloud-init/guest agent/SCSI boot, removed build NIC/identity, and cleaned temporary guests. |
| Ubuntu 24.04 OS template | [Controlled build/smoke canary](https://github.com/mutex-solutions/proxcloud/actions/runs/42732526994) succeeded with the same cleanup checks. These templates are site-local PVE objects, not public downloadable VM disks. |
| Kubernetes static node-template build | Code, synthetic failure/cleanup tests, NoCloud ISO tooling and generated Packer configuration were validated. **Real ISO boot, installed reboot, Goss and final seed cleanup have not been accepted on PVE yet.** Pick a Kubernetes version and run a controlled canary before advertising it as supported. |
| Helm charts | App/Kubernetes lint and local render checks, plus database example renders, are not proof of OCI publication, anonymous pull or clean installation from released artifacts. |
| Template updates (admin portal) | Discovery, GitHub classic PAT integration, VM import, smoke test, publish and withdraw have unit and integration tests only. The VM import and smoke test are **not lab-verified on PVE 9.2**, and the PAT integration has not been verified against the real GitHub packages. The VM executor is off unless `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`. Sandbox image mirroring (GHCR → sandbox registry, prepull switch, rollback) has unit, PostgreSQL, integration and offline CEL admission-policy tests only; it is **not lab-verified** on a real sandbox cluster with Zot and GHCR, and runs only with `PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true`. It pulls public `sandbox-*` packages anonymously (private packages are refused). Residual risks: the narrow Zot `mirror` account may create a new `library/sandbox-<name>` repository (it cannot overwrite or delete an image), and the optional, operator-applied `warming-rbac.yaml` already gives the portal's sandbox identity wider pod/Secret rights in `sandbox-system` (not a regression; [§5.3](template-updates.md#53-prepull-rbac-and-admission-policy)). See [template updates](template-updates.md#7-open-items). |
| Kubernetes node VM image | `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s` was published on 2026-11-12 as a separate, manually dispatched artifact; it is not part of the app chart or the release tag. Publication is not node acceptance: no kubeadm join or NodeReady is claimed, and the runtime checks in [the sanitization boundary](public-vm-sanitization.md#kubernetes-variant---k8s-os-ubuntu-2604) remain the acceptance gate. |
| Managed services / split installer | Existing code and historical lab records are not blanket current-main fresh-install, upgrade or all-engine acceptance. Verify the selected service/operator/chart combination. |

No environment-specific endpoint, token, node name, build address or runner
registration belongs in public documentation or release artifacts.

## Platform setup lab acceptance

On 2026-11-15 the Platform setup page (`/setup`) was run end to end on the `v0.7.0-rc.1` tag
(commit `938a5ce`) against a nested three-node Proxmox VE 9.2 cluster. The portal was a fresh Docker
Compose install of the tag with TLS mode `internal` and `--template-updates`, all containers healthy.
It used a new token, its own project network range and its own pool prefix, so the isolation rules of the
other portal on that cluster were not touched.

| Step | What was checked |
|---|---|
| Dashboard | The Platform setup card showed the fresh Compose seed as not done. |
| 1 Prepare | `GET /api/v1/admin/setup/proxmox-script` returned the script; run as root on one node, it created the token. The role it uses gained no privilege. |
| 2 Connect | Without a pin the connection failed on TLS and the answer carried the fingerprint the server presented; with that fingerprint pinned every check passed, and the GPU mapping row warned (the cluster has no GPU). The provider was saved with `attachToNewProjects: false`, and the response held no secret. |
| 3 Network | A simple zone with its own `cidrPool`. |
| 4 Storage | Detect listed the cluster's stores; the two proposed profiles were saved, with an import store and a backup store. |
| 5 OS templates | Done at once: the cluster already had three published OS images. No template was imported through the page. |
| 6 Catalog | An apply without `confirmRates` was refused (`400`). The first apply created the flavors, the price plan (7 rules, USD), the default quota and the region row; a second apply skipped all four. |
| 7 Go live | The attach policy was turned on, the provider sync was accepted (`202`), and the bulk attach had nothing to attach. Go live then showed only the warning that the job scheduler is off. |
| Tenant | A user registered in the customer realm and created an organisation and a project. The project stayed disabled until the operator activated its billing profile, then was enabled and bootstrapped (its pool was created on the cluster). A tenant network became active, and a Debian 13 server became active with its address shown in the customer console after a sync. |
| Pricing | The customer price preview of the server's flavor matched the rate card. The hourly charge job created the open bill, with no item yet (the server was younger than the charged hour). |

**Finding, fixed after the prerelease.** URL seeds cannot work on such an install: the lab VMs could not
reach the portal host, and TLS mode `internal` is self-signed. The first server booted without its seed
(the template's hostname, no guest agent). With `config.metadata.forceIso: true` the second server got its
seed (its own hostname, guest agent up). The setup page had no seed item and no warning, and the
`proxcloud-up.sh` next steps pointed at Cloud providers. The fixes are in the changelog (`[0.7.0]`, Fixed and
the server seed choice under Added). They were re-checked on the same install with images built from `main`
at `9c09945`: the network step's `seeds` item was done ("server seeds go on an ISO"), the setup status
carried the portal's real seed URL, the Network step showed the Server seeds form with the stored choice,
and the console showed no errors. Not re-checked: the `proxcloud-up.sh` next steps (the install was not run
again) and saving the seed choice from the form (`forceIso` was set before the fix).

**Not checked on the lab:** a password or SSH login into a tenant server, a public IP, an hourly bill
item (it needs the next hour), a template import through the page, the job scheduler turned on, and the
seed ISO left on the store after a server is deleted (known, see [server seed safety](server-seed-safety.md)).
This is one run on one lab, not a clean install from released artifacts (gate step 5 below), not an
installer or Helm install, and not acceptance of `v0.7.0` itself.

## Intentional safety holds and operational defaults

- **Managed sandbox saved lifecycle is held.** Pause, saved resume, paused
  connect/wake, fork and new AutoPause/AutoResume promises have no override flag.
  Named snapshot capture/export and ordinary pinned CREATE are separate. See
  [saved-runtime admission](saved-runtime-admission.md), not historical successful
  drills as evidence that the current managed lifecycle can be enabled.
- **Registry deletion is not generally enabled.** Dry-run/retained reference
  evidence is not authority to delete manifests. Follow the current
  [registry reference boundaries](durable-registry-pins.md).
- **Portal template updates are off by default.** The VM import, smoke test,
  publish and withdraw run only with `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`;
  sandbox image mirroring runs only with `PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true`
  (set it the same on every API replica). Enable either only after the lab checks in
  [template updates](template-updates.md#7-open-items). The first `make upgrade` of
  `v0.5.0` restarts the sandbox registry (Zot) pod once.
- **Cron/debug producers are off by default.** Primary PostgreSQL workers continue
  already-accepted jobs. Enabling scheduling is an explicit operator decision,
  especially for billing, resource sync and cloud mutations.
- **Mail outbox integration is bounded and opt-in.** A producer that enqueues an
  encrypted mail decision does not imply a production SMTP dispatcher is running.
  See [mail safety limits](mail-outbox-safety-limits.md) and each producer's guide.
- **Old installations need a migration decision.** The new API neither consumes
  nor imports old RabbitMQ messages. Stop/drain/reconcile and retain evidence and
  data before upgrading; never use `down -v`, broker/PVC deletion or blind rollback
  as a substitute for the [migration runbook](postgres-jobs-migration.md).
- **Demo settings are not production settings.** Plain HTTP, default development
  users/passwords and single-replica convenience services require deliberate TLS,
  identity, key management, backups and availability design before real use.

## Minimum gate for a release

The owner decided on 2026-11-12 to release `v0.4.0` as stable, and on 2026-11-13
that the next release is the stable `v0.5.0`, each with a narrowly scoped core and
explicitly held features; neither need claim every managed service. Those decisions
are not evidence of publication or installation. The releases `v0.5.1`, `v0.5.2` and `v0.6.0` (2026-11-14) and
`v0.7.0` (2026-11-15) follow the same gate. For every release:

1. Review the exact release commit for the release and its supported core surface
   and explicit experimental/held features. The version is not published until
   its tag's workflows succeed.
2. Verify app/chart/installer BOM alignment with that exact version. Keep managed
   charts independent: `database-cluster` `0.5.1` and `proxmox-kamaji-cluster`
   `0.1.12` (source; `0.1.11` is published and pinned by the platform BOM) need their
   own tag/publication checks. Do not move existing provider
   pins or managed data as a consequence of the app release.
3. Verify the release-policy tests on the selected commit. The Release job accepts
   strict stable/RC tags from the environment: RCs use
   `--prerelease --latest=false`, stable releases use `--latest`. Notes and release
   creation use only the validated tag/version and fixed policy flags.
4. Require successful tests for the exact release commit and verify **all** image
   and chart publication jobs, package visibility and anonymous pulls. The current
   image and Helm workflows are independent; image completion alone is not chart
   completion or a deployment. Keep the default `images.build: true` meanwhile.
5. After publication, install the exact released chart/images with
   `images.build: false` on a clean evaluation site (not performed for `v0.4.0`,
   and not yet performed for `v0.5.0` to `v0.7.0` when this was prepared; a portal-only Docker Compose
   site is running `v0.5.2` on a homelab, and the [Platform setup lab acceptance](#platform-setup-lab-acceptance)
   used a Docker Compose install of `v0.7.0-rc.1`; neither is this acceptance). The installer
   accepts valid SemVer with **base version >= 0.4.0**, including prereleases; this local guard
   does not prove publication or a stable release. Check login/client/admin,
   provider connection, one basic server create/delete, configuration/secrets and
   the explicit scheduler choice. A source-build installation is not this acceptance.
6. Record rollback/backup/migration limits. `v0.5.x` is stable by owner decision,
   **not** by acceptance: neither this preparation nor a green build clears full
   live installer (P6) or split/managed-service (P7) acceptance, which the owner
   deferred. Intentional safety holds stay in place.
7. Confirm the exact-commit `security-scan` gate passed. Fixable HIGH/CRITICAL
   findings block; findings without an upstream fix are tracked (not blocking)
   until 2027-03-12 ([review of 2026-11-15](security-unfixed-review.md)); the single
   VEX expires on 2026-12-31; raw upstream bases are report-only; the GPU/CUDA
   template is not scanned. The default `python` template is a public image pinned
   as is and scanned only as the report-only base `external-python`, so its findings
   never block, even with a fixed version: read `base/external-python` in the
   security comment by hand. The other built-in sandbox template digests in
   `deploy/templates/images.yaml` must come from a `sandbox-templates` build of the
   release sources.

A `v*` tag is an app publication trigger, not a PVE template trigger. Later
owner-authorized template canaries remain separate reviewed branch/run operations;
this change neither triggers them nor changes their credentials or runner routing.

## How to read older documentation

Current runbooks and code/configuration take precedence over historical lab
sections. ADRs preserve the original decision and carry supersession notes when
runtime behavior changed. The architecture changelog is history, not a current
feature enablement guide. `Implemented` in a C4 diagram means code exists; it does
not mean the path is enabled, production-accepted or released. If instructions
contradict current configuration, report the exact commit through
[GitHub Issues](https://github.com/mutex-solutions/proxcloud/issues/new/choose).
