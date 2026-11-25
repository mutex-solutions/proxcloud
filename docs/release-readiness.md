# Current support boundaries and first-release readiness

Status snapshot: **2026-11-25**, preparing the release `v0.11.1` on top of `v0.11.0`
(tag 2026-11-22). This is an evidence summary, not a release announcement or a production SLA.
`v0.4.0-rc.1` (tag 2026-11-11) was published as a non-latest prerelease, `v0.4.0` (tag 2026-11-12)
as the first stable release, `v0.5.0` (tag 2026-11-13) as the second, the patches `v0.5.1` and `v0.5.2` and the
stable `v0.6.0` (tags 2026-11-14), and `v0.7.0-rc.1` as a non-latest prerelease and the stable `v0.7.0` (tags 2026-11-15), the stable `v0.8.0` (tag 2026-11-17), the patches `v0.8.1` and `v0.8.2` (tags 2026-11-18), and `v0.9.0-rc.1` and `v0.9.0-rc.2` as non-latest prereleases, the stable `v0.9.0` and the patch `v0.9.1` (tags 2026-11-19), and `v0.10.0-rc.1` as a non-latest prerelease, the stable `v0.10.0`, and `v0.10.1-rc.1` and `v0.10.1-rc.2` as non-latest prereleases (tags 2026-11-20), and the stable `v0.10.1`, `v0.10.2-rc.1` as a non-latest prerelease, the stable `v0.10.2` and `v0.11.0-rc.1` as a non-latest prerelease (tags 2026-11-21), and `v0.11.0-rc.2` as a non-latest prerelease and the stable `v0.11.0` (tags 2026-11-22), and `v0.11.1-rc.1` (tag 2026-11-23), `v0.11.1-rc.2` and `v0.11.1-rc.3` (tags 2026-11-24) and `v0.11.1-rc.4` (tag 2026-11-25) as non-latest prereleases. Pushing the `v0.11.1` tag
publishes the stable images, app chart and GitHub Release. `v0.5.1` to `v0.11.1` have no breaking API change against
`v0.5.0` (`v0.6.0` changes the VM image size contract; `v0.7.0` stops serving the management port job triggers with
only the scheduler on and refuses a new problem in a Proxmox provider's network block on save; `v0.8.0` ignores a
node group's `imageId`, answers a VLAN-zone tenant network without DHCP and SNAT, and changes the Proxmox grant
that mints cluster tokens, which existing managed Kubernetes sites must migrate; `v0.8.1` deletes a security
group rule by `ruleId` only, refuses rule fields it cannot store and `enableDhcp: true` in a zone without a DHCP
server; `v0.8.2` adds answers and takes none away; `v0.9.0` reserves security group names that start with `k8s-`,
refuses `spec.sandbox` and a preset on an installed platform, and checks the worker repair and worker firewall
settings of a Kubernetes provider on save; `v0.9.1` changes no answer of the API; `v0.10.0` adds answers and takes none away; `v0.10.1` refuses a network with host bits in a field that ends in Kubernetes and adds answers; `v0.10.2` adds answers and takes none away; `v0.11.0` adds answers and builds a new load balancer on two appliances unless the request says `ha: false`; `v0.11.1` adds answers, bills the address of a cluster's Service from its allocation where it had no floating IP row, and answers no ICMP "port unreachable" for a UDP port without a listener on a pair with a UDP listener; see the changelog);
`v0.5.0` contains breaking API changes against `v0.4.0`; see the [changelog](../CHANGELOG.md). This source change publishes
nothing and performs no live acceptance; the [lab acceptance of Platform setup](#platform-setup-lab-acceptance)
was run on `v0.7.0-rc.1`, and the [managed services lab rounds](#managed-services-lab-rounds) on builds of
`main` before `v0.8.0`, for `v0.8.1` and `v0.8.2` on the released `v0.8.0` followed by a build of `main`, and for `v0.9.0` on a build of `main` and on its two release candidates; the fixes of `v0.9.1` were not run on a lab; `v0.10.0` on its release candidate `v0.10.0-rc.1`; `v0.10.1` on its two release candidates, without the fixes merged after the second; `v0.10.2` on its release candidate `v0.10.2-rc.1`; `v0.11.0` on its two release candidates; `v0.11.1` on its four release candidates. Check GitHub's Releases/Actions pages before choosing a deployment
commit.

## What is available now

- Source on `main`, public development container images on GHCR, the
  `v0.4.0-rc.1`, `v0.7.0-rc.1`, `v0.9.0-rc.1`, `v0.9.0-rc.2`, `v0.10.0-rc.1`, `v0.10.1-rc.1`, `v0.10.1-rc.2`, `v0.10.2-rc.1`, `v0.11.0-rc.1`, `v0.11.0-rc.2`, `v0.11.1-rc.1`, `v0.11.1-rc.2`, `v0.11.1-rc.3` and `v0.11.1-rc.4` prereleases and the stable releases `v0.4.0`, `v0.5.0`, `v0.5.1`, `v0.5.2`,
  `v0.6.0`, `v0.7.0`, `v0.8.0`, `v0.8.1`, `v0.8.2`, `v0.9.0`, `v0.9.1`, `v0.10.0`, `v0.10.1`, `v0.10.2` and `v0.11.0` (images and app charts of the same versions). The
  app chart `0.11.1`, appVersion and installer BOM `v0.11.1` and the platform BOM `0.11.1` name the current stable release; its tag publishes those artifacts.
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

## Managed services lab rounds

`v0.8.0` carries the managed-services platform (ADR 0009). Its lab record is kept in the operator guide, section
[Not verified on a lab yet](managed-services.md#not-verified-on-a-lab-yet): the rounds ran on builds of `main`
(the last on 2026-11-17, `cb2f052`), on one lab Proxmox VE cluster. In short, from that section: the install
finished (the second time in one run without a Retry, on cluster chart 0.1.13 and platform chart 0.1.2), Remove
of a ready platform and from a failed stage 7 passed, a tenant cluster on a VLAN zone had three workers Ready,
two clean seed reboots passed on the fixed image, and one seed restore passed. The checks L0 to L11 of the design
are not recorded as passed, no tenant database was recorded healthy, and nothing was run on the published
artifacts of `v0.8.0` in those rounds: this is not acceptance of the release (gate step 5 below).

For `v0.8.1` one more round ran on 2026-11-18. The lab portal was set to the released `v0.8.0` (its published
images, through `proxcloud-up.sh upgrade v0.8.0`), a platform was installed from the 0.8.0 BOM in one run without
a Retry, and the portal was then upgraded to the build `dev-751a190` of `main`, which is the code of `v0.8.1`
without the release commit and the changelog. The platform stayed on the 0.8.0 BOM: the plan had no change, the
Check was green with its two new rows, and **Apply this release** succeeded in 6.6 seconds without touching the
seed's Applications. The fixes of `v0.8.1` were then run one by one; what passed and what was not run (volume
attach, detach and rebuild among them) is in the changelog section of `v0.8.1`. Nothing was run on the published
artifacts of `v0.8.1` itself on the lab.

For `v0.8.2` the same lab platform (installed by the released `v0.8.0`) was used on 2026-11-18. The portal was
upgraded to the build `dev-03bc9f3` of `main`, which is the code of `v0.8.2` without the sandbox template image
pins (they are in no image), the fix of the break-glass port-forward rule, the changelog and the release commit.
The platform stayed on the 0.8.0 BOM with no change in the plan, and the Check passed with its new row
`tenant-path`. The row was read in its ok case (a VLAN zone) and its warning case (a `simple` zone); a joined
one-worker tenant cluster was deleted and its Proxmox token and ACL rows were revoked by the release job 2
minutes 53 seconds later without a provider sync; the platform kubeconfig and both ArgoCD commands of the
ready card worked. Of the customer console fixes only two were run, at the API: the lab node that holds the OS
image had no capacity for a tenant server. What passed and what was not run is in the changelog section of
`v0.8.2`. Nothing was run on the published artifacts of `v0.8.2` itself. Outside the lab, one production site
was upgraded to the released `v0.8.1` on 2026-11-18: its installed platform stayed on its BOM (a plan with no
change, an apply that succeeded, `onboot` set on the platform's machines).

For `v0.9.0` the lab ran three rounds on 2026-11-19. The first, on the build `dev-68a15e8` of `main` over the
platform that `v0.8.0` installed, found the defects that the fixes of this release close. The second, on the
published images of the candidate `v0.9.0-rc.1`, moved that platform from the BOM `0.8.0` to `0.9.0-rc.1` in one
run of 72.8 seconds with no machine replaced, on a platform without a tenant cluster or a database, and ran the
features of cluster chart 0.1.14 on new tenant clusters. The third, on the published images of the candidate
`v0.9.0-rc.2`, removed that platform, installed a new one in bridge mode in one run of 40 minutes, and used it
as a tenant: a cluster of two workers, a load balancer, a database, and the tenant path test with the verdict
`passed`. Not run on a lab: the upgrade on a platform with tenants, the published artifacts of `v0.9.0` itself,
the one root command on a fresh token set, the Homelab preset as an install, `routed` mode, a replacement by the
health check, three control plane machines and the console pages. The full lists are in the
[release notes of v0.9.0](releases/v0.9.0.md).

For `v0.9.1` no lab round was run: its fixes are covered by tests alone. Outside the lab, one production site
with one tenant cluster and one database was upgraded to the released `v0.9.0` on 2026-11-19 and its platform
moved from the BOM `0.8.0` to `0.9.0` in one run of 68.6 seconds with no machine replaced; three of the fixes of
`v0.9.1` come from that run. What is not verified is listed in the
[release notes of v0.9.1](releases/v0.9.1.md).

For `v0.10.0` the lab ran one round on 2026-11-20, on the published images of the candidate `v0.10.0-rc.1`: the
portal upgrade from `v0.9.1` with the PostgreSQL and Keycloak containers kept, the move of the platform from the
BOM `0.9.1` to `0.10.0-rc.1` in one run of 22.8 seconds with no machine replaced (one tenant server present, no
tenant cluster or database), the graphical console, Delete of a withdrawn template, the ArgoCD address on a layout
without SNAT, and a tenant cluster on chart 0.1.15. Not run on a lab: the upgrade of the seed's k3s and the
replacement of the platform cluster's machines (this BOM moves neither k3s nor Kubernetes), the fixes merged after
the candidate, the published artifacts of `v0.10.0` itself and any move to the BOM `0.10.0`. The full lists are in
the [release notes of v0.10.0](releases/v0.10.0.md).

For `v0.10.1` the lab ran two drills on 2026-11-20 and 21, on the published images of its candidates: the move of a
platform from the BOM `0.10.0` to Kubernetes 1.36.6, with one tenant cluster and one database under a write loop.
On `v0.10.1-rc.1`: the part "Seed and node image" (the seed's k3s to `v1.36.6+k3s1`, the seed's API away 43 seconds)
and the control plane's unit, after which the run stood. On `v0.10.1-rc.2`: one Retry that took that platform on,
the units `system`, `kamaji` and `dbaas`, Pause and Resume, and a new tenant cluster at 1.36.6. Not run on a lab:
the fix of the wait for the tenants between two units merged after the second candidate, the published artifacts of
`v0.10.1` itself, any move to the BOM `0.10.1`, a one-node site, a site with SNAT or a routed tenant path, a unit
replaced delete-first, the seed's way back by its snapshot, and the moves from BOMs older than `0.10.0`. The full
lists are in the [release notes of v0.10.1](releases/v0.10.1.md).

For `v0.10.2` the lab ran one drill on 2026-11-21, on the published images of its candidate `v0.10.2-rc.1`: the move
of a platform from the BOM `0.10.1`, cert-manager `v1.20.2` to `v1.21.3` on the seed and on the platform cluster,
with one tenant cluster and one database probed. One run of 3 minutes 8 seconds; no Certificate issued again, no
webhook refusal, no tenant probe failed, nothing written on Proxmox; afterwards a new and a renewed Certificate on
1.36. Not run on a lab: the installer path, the moves from `0.10.1-rc.1` and `0.10.1-rc.2`, a site with the images
already cached, a failed `startupapicheck` and the Retry after it, a seed of more than one node, a production site,
the published artifacts of `v0.10.2` itself and any move to the BOM `0.10.2`. The full lists are in the
[release notes of v0.10.2](releases/v0.10.2.md).

For `v0.11.0` the lab ran two drills, each followed by a read-only audit: on the published images of
`v0.11.0-rc.1` (2026-11-21) and of `v0.11.0-rc.2` (2026-11-22). One lab, every appliance on one Proxmox node, Ubuntu
26.04 appliances, a public test range on a plain bridge, every step through the API. The first drill passed the
pair, 15 failovers of 0.7 to 3.5 s, a cluster's controller, the refusals, billing rows and clean-up, and found two
faults that blocked the release: Replace appliances left an address refusing connections for 458 s, and a stopped
master appliance was started again in 1 of 6 tries. The second drill showed both corrected (three replaces under
load with one switch of 0.7 to 0.8 s each; a stopped master started by the next sync three times of three), the
upgrade between the candidates pushing no balancer that served, and a cluster's listeners following a Service's
ports with no request lost on the other listener. It also measured what the release lists as known issues: requests
lost for seconds when a worker is added or removed, and at each reload for a policy `Local` Service on a cluster
with a node that has no endpoint. Not run on a lab: two nodes (VRRP across nodes, the loss of a node), the consoles,
a production site, the default Debian 13 appliance image, a balancer made before `v0.11.0` across the upgrade, the
scheduler, the installer path, the sandbox node fix, the published artifacts of `v0.11.0` itself and any move to the
BOM `0.11.0`. UDP listeners and TLS termination are not in this release. The full lists are in the
[release notes of v0.11.0](releases/v0.11.0.md).

For `v0.11.1` the same lab ran four drills, each followed by a read-only audit: on the published images of
`v0.11.1-rc.1` (2026-11-23), `v0.11.1-rc.2` (2026-11-23 and 2026-11-24), `v0.11.1-rc.3` (2026-11-24) and
`v0.11.1-rc.4` (2026-11-25). One lab, every appliance on one Proxmox node, Ubuntu 26.04 appliances, the scheduler
off. The first drill ran the upgrade from `v0.11.0` (31365 requests, 0 failed), TLS on a single balancer and a
cluster's move to the new controller, and found that a pair with a UDP listener read as degraded. The second ran
Kubernetes Services and a tenant's own pair with UDP and TLS, and found three faults: a change waited while the
image's package manager ran, a changed UDP connection limit did not reach the kernel, and an address annotated
just before its delete was given back. The third showed those corrected and found that a UDP client that was
sending before its listener existed got no answer. The fourth showed that corrected (answered within 0.2 s of the
listener being served, in six cases) and found no defect; its audit's verdict was that the candidate can become
the release unchanged. Each upgrade between two neighbours ran under client loops with 0 failed requests. Not run
on a lab: the published artifacts of `v0.11.1` itself and any move to the BOM `0.11.1`, the upgrade from `v0.11.0`
straight to the release, Debian 13 appliances, two nodes, a node change under load, the consoles, a production
site, the scheduler, a rollback of the portal, and what a charge run bills for the rows a pass wrote. The full
lists are in the [release notes of v0.11.1](releases/v0.11.1.md).

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
are not evidence of publication or installation. The releases `v0.5.1`, `v0.5.2` and `v0.6.0` (2026-11-14),
`v0.7.0` (2026-11-15), `v0.8.0` (2026-11-17), `v0.8.1` and `v0.8.2` (2026-11-18), `v0.9.0` and `v0.9.1` (2026-11-19), `v0.10.0` (2026-11-20), `v0.10.1` and `v0.10.2` (2026-11-21), `v0.11.0` (2026-11-22) and `v0.11.1` follow the same gate. For every release:

1. Review the exact release commit for the release and its supported core surface
   and explicit experimental/held features. The version is not published until
   its tag's workflows succeed.
2. Verify app/chart/installer BOM alignment with that exact version. Keep managed
   charts independent: `database-cluster` `0.5.2`, `proxmox-kamaji-cluster`
   `0.1.19` (new tenant clusters; the platform cluster stays on `0.1.16`) and `proxcloud-platform` `0.1.3`
   (published and pinned by the platform BOM)
   need their own tag/publication checks. A newer version of one of them needs its own tag
   and then its digest in the platform BOM before an app release names it. Do not move existing provider
   pins or managed data as a consequence of the app release.
3. Verify the release-policy tests on the selected commit. The Release job accepts
   strict stable/RC tags from the environment: RCs use
   `--prerelease --latest=false`, stable releases use `--latest`. Notes and release
   creation use only the validated tag/version and fixed policy flags.
   The release pull request holds `docs/releases/vX.Y.Z.md`, the Release body,
   next to the complete `CHANGELOG.md` section
   ([release notes](ci-release.md#release-notes)).
4. Require successful tests for the exact release commit and verify **all** image
   and chart publication jobs, package visibility and anonymous pulls. The current
   image and Helm workflows are independent; image completion alone is not chart
   completion or a deployment. Keep the default `images.build: true` meanwhile.
5. After publication, install the exact released chart/images with
   `images.build: false` on a clean evaluation site (not performed for `v0.4.0`,
   and not yet performed for `v0.5.0` to `v0.11.1` when this was prepared; a portal-only Docker Compose
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
