# CI, template builds and release operations

This page describes the checked-in workflows, not a guarantee that a release has
already been published. As of **2026-11-15**, GHCR has public development images,
the `v0.4.0-rc.1` prerelease (tag 2026-11-11, images and app chart `0.4.0-rc.1`)
and the first stable release `v0.4.0` (tag 2026-11-12, images and app chart
`0.4.0`), then the stable `v0.5.0` (tag 2026-11-13), the patches `v0.5.1` and `v0.5.2` and the stable `v0.6.0` (tags 2026-11-14),
and the `v0.7.0-rc.1` prerelease (tag 2026-11-15). The next release is the stable
`v0.7.0`; pushing its tag runs the app image/chart publication and GitHub Release
creation.
This source change does none of those operations, and installation from released
artifacts and later controlled PVE template canaries remain separate steps. Read
[release readiness](release-readiness.md) before publication or evaluation.

## Workflow map

| Workflow | Triggers | Effect / runner |
|---|---|---|
| `test.yml` | PRs targeting `main`; pushes to `main` | Hosted Go unit/integration tests, both SPA build/smoke tests, installer syntax/tests, credential-free Packer validation and offline image-pipeline tests, naming gates. Not triggered by release tags. |
| `security-scan.yml` | PRs targeting `main`; pushes to `main` | Exact-head Trivy 0.76.2 scans of lockfiles, six local product images, eleven non-GPU template outputs, thirteen raw upstream bases (report-only) and a nondefault controlled-base prototype; required `security-gate` job and a PR summary comment. Nothing is pushed. Not triggered by release tags. |
| `docker.yml` | PRs to `main`, `main` pushes, `v*` tags, manual dispatch | Job `bom-verify` runs first; on a tag it is the [platform BOM release gate](#platform-bom-verification-and-release-gate) and the builds wait for it. PRs build five images without pushing. Image-relevant `main` pushes publish `dev-<7-char-sha>`; docs/chart-only changes can skip. Tags publish exact image tags; manual dispatch pushes `dev-<sha>` or `dev-<suffix>`. Hosted runner. |
| `helm.yml` | Chart-path PRs to `main`; app or managed-chart tags | PR lint/render only. App `v*` publishes `proxcloud`; `database-cluster-v*`, `proxmox-kamaji-cluster-v*` and `proxcloud-platform-v*` publish their independent charts. Hosted runner; no publication on ordinary merge. |
| `bom-verify.yml` | PRs to `main` that touch `deploy/platform/**` or the script; weekly schedule; manual dispatch | Compares every pin of the platform BOM with its registry by anonymous pull. Read-only token, no secret, nothing is pushed. Hosted runner. |
| `sandbox-templates.yml` | Selected source pushes/PRs, schedule, manual dispatch | PR read-only build; trusted runs push built-in images, then `main` opens a digest-pin PR. Not cluster deployment. Hosted runner. |
| `vm-export-tools.yml` | Tool-image recipe pushes/PRs, manual dispatch on `main` | PR builds, smokes and scans the VM export tool image with a read-only token. `main` then pushes it to GHCR under one tag it never replaces and records the digest; `capsule.py` pins one reviewed digest and the export job pulls it. Hosted runner. |
| `clusterctl.yml` | Image-recipe PRs to `main`; `proxcloud-clusterctl-v*` tags | PR builds and scans `proxcloud-clusterctl` (clusterctl and kubectl built from the upstream source of their release tags with patched `golang.org/x/net` and `golang.org/x/text`, baked provider manifests) with a read-only token. A tag that equals the image's version label and points at a commit of `main` pushes it once to GHCR under that version, refusing an existing tag, and records the digest. Hosted runner. |
| `vm-artifact-export.yml` | Manual dispatch on `main` only: `variant`, `os`, `kubernetes_version`, `template_vmid` | One run from a lab template to a published image. `export` (self-hosted, label `proxcloud-vm-artifact-export`, no registry credential): template guard, read-only copy, inventory, review, sanitize, postscan, cold boot, bundle, public evidence. Then hosted: `publish` (calls `vm-artifacts.yml`), `attest` (build provenance for the pushed digest, verified in the job, and for the evidence record's sha256) and `verify` (anonymous pull, no credential). No PR/fork trigger; the export job uses the environment `vm-artifact-export`, and its runner must be restricted to this workflow on `main` (owner settings, not expressible here). See [VM artifact export and publishing](vm-artifact-publishing.md). |
| `vm-artifacts.yml` | Called by `vm-artifact-export.yml` only (`workflow_call`); no manual or event trigger | The reusable publisher: accepts only the successful `export` job of the calling run and only for what was dispatched, re-verifies the three files, refuses an existing tag, pushes with ORAS and records the digest of that push. Hosted runner. |
| `vm-templates.yml` | Selected image-source pushes to `main`, weekly schedule, manual dispatch | Builds Debian/Ubuntu templates directly in PVE and publishes by pool/tags. Privileged self-hosted runner; no PR/fork trigger. |
| `k8s-node-templates.yml` | Manual dispatch, required Kubernetes `version` | Builds node templates; `network_mode` defaults `static`. Privileged self-hosted runner; no PR/fork trigger. |

The source of truth is [.github/workflows](../.github/workflows/). Image-relevant
path filters and per-job conditions matter: a green skipped Docker workflow does
not prove new images were pushed for that commit.

## Container images and chart versions

Five app-release image repositories are used:

- `ghcr.io/mutex-solutions/proxcloud-api`
- `ghcr.io/mutex-solutions/proxcloud-web`
- `ghcr.io/mutex-solutions/proxcloud-admin`
- `ghcr.io/mutex-solutions/proxcloud-sandbox-proxy`
- `ghcr.io/mutex-solutions/proxcloud-sandbox-runtime`

Do not assume `latest` or the Compose-only `dev` tag is a CI output. Pin an
actually published version/digest; source `main` image tags use `dev-<short-sha>`.
No image is produced for some documentation/chart-only commits.

The configured destination for app Helm publication is
`oci://ghcr.io/mutex-solutions/charts` (it holds the `0.4.0-rc.1` prerelease and
`0.4.0` stable charts).
On an app release tag, the workflow sets chart version to the tag without `v` and
appVersion to the full tag, matching the image tags. The release contract is app
chart `0.7.0`, appVersion/image tag and installer BOM `v0.7.0`, and the same tag as
`DEFAULT_VERSION` of the Docker Compose install (`deploy/compose/proxcloud-up.sh`).
Keep `images.build: true` until the exact chart and images are published and
verified; use `false` explicitly for released-artifact evaluation after that.
The installer's broker-free floor compares the SemVer **base** against `0.4.0`:
valid prereleases and build metadata are accepted syntactically, not verified as
published. App publication tags are narrower (stable/RC, no build metadata), since
images use the raw tag and Docker tags cannot contain `+`.

Managed charts use their own metadata versions and reject a mismatching tag.
`database-cluster` `0.5.1` and `proxmox-kamaji-cluster` `0.1.11` need separate
publication checks/tags; the app tag does not publish them or move existing
provider pins or managed data. Re-publishing an already existing chart version is
skipped; it does not replace old contents. See the
[chart overview](../deploy/charts/README.md).

`make image` / `make push` build/push **only the API**. `make deploy` sets only
`api.image.tag`; it does not roll matching frontend/sandbox images automatically.
For a coordinated development deployment, set all required image pins explicitly
or use an actually published versioned chart. These commands mutate an image
registry or cluster; they are not CI verification steps.

## Permissions and secrets

Normal test/container/chart CI does not need a manually created GHCR PAT, PVE
credential or kubeconfig. GitHub supplies `GITHUB_TOKEN`; publish jobs request
`packages: write`, and release creation requests `contents: write`. Keep the
repository token default read-only and grant only necessary job permissions.
Existing packages must grant the workflow repository access. A public repository
does not automatically make every newly created GHCR package public: verify each
package's visibility and anonymous pull behavior.

The built-in-template pin job pushes a branch and opens a PR. It requires the
Actions policy **Allow GitHub Actions to create and approve pull requests** at the
applicable organization/repository levels, plus its declared PR/content rights.
A token-created pin PR may need a reviewed GitHub App/manual check path if normal
PR checks are not triggered; never bypass this by running unreviewed code under
`pull_request_target` with privileged credentials.

PVE template jobs use repository Actions **Secrets** for all cluster values; the
placeholder-only [configuration example](../.github/pve-actions.env.example)
explains exact keys. No GitHub Environment is wired into these jobs; environment
secrets alone will not be consumed without a workflow change. Secrets masking is
best-effort and does not sanitize artifacts, substrings or transformed values.
The OS manifest intentionally exports only `os`, `built`, `version`.

## Hosted versus privileged self-hosted runners

Standard hosted runners for public repositories do not consume the private-repo
included compute allowance. Larger runners/storage/billing-account restrictions
are separate; see [GitHub Actions billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions).
Public packages are free to use under the current
[Packages billing policy](https://docs.github.com/en/billing/concepts/product-billing/github-packages).
These policies are not permission to assume unlimited storage or ignore an account
payment restriction.

The PVE scripts orchestrate VMs on Proxmox; the runner itself need not run nested
virtualization. It does need PVE API access, SSH routing to the build guest and
appropriate tooling. OS builds use a reserved static build IP; Kubernetes static
mode uses a private local seed ISO with xorriso/OpenSSL and scoped ISO rights.
Legacy DHCP mode additionally needs guest-to-runner HTTP reachability and trusted
DHCP on the guest network. A routed runner is not automatically on that VLAN's L2.

**Never route ordinary public-PR builds onto a persistent PVE runner.** Labels are
routing hints, not authorization boundaries. Two systemd users or containers on
one host do not isolate a compromised host/kernel or privileged network. Protect
workflow changes and restrict runner access; a private ops repository is safer
for credentials with cloud mutation rights. For the export runner the concrete
settings (a runner group restricted to `vm-artifact-export.yml` on `main`, approval
for every outside contributor's fork pull request, protection of `main`, lab secrets
in an environment limited to `main`) are listed under "Trust model" in
[VM artifact export and publishing](vm-artifact-publishing.md#trust-model). A
compromised export or template runner can publish a bad image with a valid
attestation, so these settings are part of the release process, not optional hardening.

Both PVE template workflows and the `export` job of `vm-artifact-export.yml` share
concurrency group `vm-templates`, intentionally serializing their shared
network/build resource use. Two registered runners do
not imply two concurrent template builds are safe. Before enabling final routing
labels, review/cancel stale queued runs: assigning a matching runner can start an
old workflow immediately. Runner registration must not accidentally execute an
all-OS scheduled run when only one canary was approved.

## Platform BOM verification and release gate

`deploy/platform/platform-bom.yaml` pins everything the managed-services platform
runs. [`bom_verify.py`](../.github/scripts/bom_verify.py) (standard library only,
no credentials) prints one line per pin and exits 1 on any failure:

| Pin | What is compared |
|---|---|
| OCI chart, container image, VM image | Anonymous pull token, then `HEAD` of the tag's manifest: its `Docker-Content-Digest` header must equal the pin. |
| Chart in an `https://` repository | The repository's `index.yaml` is fetched; the `digest` of the entry with that exact name and version must equal the pin. |
| k3s binary and install script (`url` + `sha256`) | The file is downloaded (at most `--max-download-mb`, default 256) and hashed. |

A `pending` digest has nothing to compare and is printed as `skip`. A mismatch, a
missing digest header, a missing tag or index entry, a download over the size
cap, a pin the script cannot read, and a network error after three attempts all
fail the run.

Where it runs:

- `bom-verify.yml`, on pull requests that touch the BOM, the script or its
  tests, and every Monday on `main`. The schedule was chosen over a run on each
  `main` push: the BOM only changes through a pull request, which already ran
  the check, while a tag or index entry moved upstream is independent of our
  pushes. GitHub disables a schedule after 60 days without repository activity.
- `docker.yml`, job `bom-verify`, on every `v*` tag with `--release`. This also
  fails on a `-dev` release, a `pending` digest and a `0.0.0+` version. `build`
  needs this job and `release` needs `build`, so a failing gate publishes no
  image and creates no GitHub Release. Off a tag the job only runs the script's
  offline unit tests; it exists on every event because a skipped job would skip
  the builds that need it.

`v0.8.0` cannot be tagged yet: the BOM is `0.8.0-dev`, and the `kamaji` and
`kamaji-crds` charts are `0.0.0+latest` with a `pending` digest. The tag passes
once the chart mirror pins both and the release drops `-dev`. The same applies
to a `v0.8.0-rc.N` tag.

Not verified:

- The content behind a digest: OCI layers are not pulled and chart archives of
  `https://` repositories are not downloaded. Only the digest that the registry
  or the index states is compared.
- Signatures and provenance of third-party charts and images.
- That the BOM's `release` equals the tag. Only `-dev` is refused.
- The app chart: `helm.yml` publishes `proxcloud` on the same `v*` tag in
  parallel and does not wait for the gate. The chart does not contain the BOM;
  the API image does.
- Whether a pinned version is still the wanted one. The check proves a pin is
  still served, not that it is current.

## Current release-gate limitations

The workflows currently **do not** provide a single atomic release gate:

- Docker publication, `test.yml` and `security-scan.yml` are independent. Tag
  pushes run neither tests nor scans; the only check they run before the image
  builds is the platform BOM gate above. Choose an exact commit whose tests and
  security gate completed successfully.
- GitHub Release creation waits for all five image builds, but not for the
  separate Helm publication workflow. Verify both before telling users to install.
- The Release job validates the tag from the environment with the stdlib
  [release policy](../.github/scripts/release_policy.py). Only `vX.Y.Z` and
  `vX.Y.Z-rc.N` (no leading zeroes) pass. RCs use `--prerelease --latest=false`;
  stable releases use `--latest`. The same policy validates tags before image
  build/push and app-chart publication; invalid tags fail before those effects or
  notes/release creation. Existing GitHub Releases are skipped, not silently
  reclassified.
- No workflow automatically installs/upgrades a production site. Publication is
  not deployment, acceptance, backup verification or rollback readiness.

The owner's stable `v0.5.x` decision does not replace exact-commit checks or
evidence, and it does not claim live installer or split/managed-service acceptance.
A `v*` tag triggers the app Docker and Helm workflows, not either PVE template
workflow, the sandbox template build or the VM image export (a manual dispatch). Keep later template
canaries on their separately reviewed branch/run path; do not change credentials,
secrets or runner routing as part of the app release. Routine docs updates must
not create tags. Use the checklist in
[release readiness](release-readiness.md#minimum-gate-for-a-release).
