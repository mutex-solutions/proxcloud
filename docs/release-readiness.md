# Current support boundaries and first-release readiness

Status snapshot: **2026-11-12**, preparing the first stable `v0.4.0` release
(owner decision 2026-11-12). This is an evidence summary, not a release
announcement or a production SLA. `v0.4.0-rc.1` (tag 2026-11-11) was published as
a non-latest prerelease. Pushing the `v0.4.0` tag publishes the stable images, app
chart and GitHub Release. This source change publishes nothing and performs no live
acceptance. Check GitHub's Releases/Actions pages before choosing a deployment
commit.

## What is available now

- Source on `main`, public development container images on GHCR and the
  `v0.4.0-rc.1` prerelease (images and app chart `0.4.0-rc.1`). The app chart
  `0.4.0`, appVersion and installer BOM `v0.4.0` name the first stable release;
  its tag publishes those artifacts. Verify they are pullable before using
  `images.build: false`; a source pin is not proof that a registry artifact exists.
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
| Managed services / split installer | Existing code and historical lab records are not blanket current-main fresh-install, upgrade or all-engine acceptance. Verify the selected service/operator/chart combination. |

No environment-specific endpoint, token, node name, build address or runner
registration belongs in public documentation or release artifacts.

## Intentional safety holds and operational defaults

- **Managed sandbox saved lifecycle is held.** Pause, saved resume, paused
  connect/wake, fork and new AutoPause/AutoResume promises have no override flag.
  Named snapshot capture/export and ordinary pinned CREATE are separate. See
  [saved-runtime admission](saved-runtime-admission.md), not historical successful
  drills as evidence that the current managed lifecycle can be enabled.
- **Registry deletion is not generally enabled.** Dry-run/retained reference
  evidence is not authority to delete manifests. Follow the current
  [registry reference boundaries](durable-registry-pins.md).
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

The owner decided on 2026-11-12 to release `v0.4.0` as stable, with a narrowly
scoped core and explicitly held features; it need not claim every managed service.
That decision is not evidence of publication or installation. For `v0.4.0` (and
any later release):

1. Review the exact release commit for `v0.4.0` and its supported core surface
   and explicit experimental/held features. The version is not published until
   its tag's workflows succeed.
2. Verify app/chart/installer BOM alignment with that exact version. Keep managed
   charts independent: `database-cluster` `0.5.1` and `proxmox-kamaji-cluster`
   `0.1.10` need their own tag/publication checks. Do not move existing provider
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
   `images.build: false` on a clean evaluation site (not yet performed for
   `v0.4.0` when this was prepared). The installer accepts valid
   SemVer with **base version >= 0.4.0**, including prereleases; this local guard
   does not prove publication or a stable release. Check login/client/admin,
   provider connection, one basic server create/delete, configuration/secrets and
   the explicit scheduler choice. A source-build installation is not this acceptance.
6. Record rollback/backup/migration limits. `v0.4.0` is stable by owner decision,
   **not** by acceptance: neither this preparation nor a green build clears full
   live installer (P6) or split/managed-service (P7) acceptance, which the owner
   deferred. Intentional safety holds stay in place.
7. Confirm the exact-commit `security-scan` gate passed. Fixable HIGH/CRITICAL
   findings block; findings without an upstream fix are tracked (not blocking)
   until 2026-12-12; raw upstream bases are report-only; the GPU/CUDA template is
   not scanned. Built-in sandbox template digests in `deploy/templates/images.yaml`
   must come from a `sandbox-templates` build of the release sources.

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
