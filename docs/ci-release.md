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
| `charts-mirror.yml` | PRs to `main` that touch the mirror manifest, the BOM, the script or its tests; `charts-mirror-v*` tags | PR runs the script's tests and a dry run with a read-only token: every third-party chart of the BOM is fetched, verified against its pin and packaged, nothing is pushed. A tag on a commit of `main` pushes each chart version that [the chart mirror](#chart-mirror) does not serve yet to `ghcr.io/mutex-solutions/charts-mirror`, never replaces a served version, and records the digests. Hosted runner. |
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
cap, a pin the script cannot read, a `release` line that is not one plain
version, and a network error or cut response after three attempts all fail the
run. A manifest request is never redirected, so the pull token stays with the
registry that issued the challenge; a file download follows `https://`
redirects only.

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
- `helm.yml`, job `publish`, on every `v*` tag with `--release`, as a step
  before the registry login. A published chart version is never replaced, so a
  tag that fails the gate must not publish the `proxcloud` chart either.

`v0.8.0` cannot be tagged yet: the BOM is `0.8.0-dev`, and the `kamaji` and
`kamaji-crds` charts are `0.0.0+latest` with a `pending` digest. The tag passes
once the BOM takes both from [the chart mirror](#chart-mirror) at an exact
version and digest and the release drops `-dev`. The same applies to a
`v0.8.0-rc.N` tag.

Not verified:

- The content behind a digest: OCI layers are not pulled and chart archives of
  `https://` repositories are not downloaded. Only the digest that the registry
  or the index states is compared.
- Signatures and provenance of third-party charts and images.
- That the BOM's `release` equals the tag. Only `-dev` is refused.
- That both workflows see the same answer. `docker.yml` and `helm.yml` run the
  gate separately on the same tag; a registry error that hits only one of them
  fails that one and not the other. A BOM that is `-dev`, `pending` or `0.0.0+`
  fails both. `helm.yml` still does not wait for the image builds.
- The chart index parser against anything but the layout Helm writes. It reads
  `index.yaml` by indentation, not with a YAML library.
- Whether a pinned version is still the wanted one. The check proves a pin is
  still served, not that it is current.

## Chart mirror

The platform's third-party charts are republished, public, under
`oci://ghcr.io/mutex-solutions/charts-mirror/<chart>` (owner decision of
2026-11-16), so that ArgoCD can pull from one OCI registry what CI verified, and
so that Kamaji has an exact chart version. Only GitHub Actions publishes there.
The mirror flow exists; **nothing has been published yet and the BOM does not
use the mirror yet**.

`deploy/platform/charts-mirror.yaml` lists every third-party chart of the BOM
with its source. A test (`test_charts_mirror.py`) keeps it equal to the BOM:
the same charts, versions and digests, and no chart the BOM does not use.
[`charts_mirror.py`](../.github/scripts/charts_mirror.py) (standard library,
`helm` and `git`) handles each entry by its kind:

| Source | What is fetched and checked | What is published |
|---|---|---|
| `https://` repository | `helm pull` of the exact version; the sha256 of the `.tgz` must equal the pinned index digest. | The downloaded archive, unchanged. |
| OCI repository | `helm pull` by the pinned manifest digest. | The pulled archive, unchanged. |
| Upstream git tag (`kamaji`, `kamaji-crds`) | `git fetch` of the tag, which must still point at the pinned commit; no symlink in the chart directory; the chart's dependencies must be exactly the one the manifest pins by sha256, or none. | `helm package --version --app-version` of the chart directory. |

In all three cases the name and version inside the archive must be those of the
manifest. Fetching is anonymous: those `helm` calls get an empty registry
configuration and `git` runs without configuration. All charts are verified and
compared with the mirror before the first push.

Kamaji: upstream publishes its chart for edge releases only as the moving
version `0.0.0+latest`. The mirror packages `charts/kamaji` and
`charts/kamaji-crds` from the upstream tag `26.11.2-edge` (commit
`31fed7a654b361ff8332a79184e6b0d2a05eb343`), the tag of the image the BOM pins,
as chart version and `appVersion` `26.11.2-edge`. That is a valid SemVer
version without `+`, equal to the upstream tag, and it makes the chart's
default image the pinned one. The `kamaji` chart declares `kamaji-etcd` as a
dependency and locks `0.15.0`; that archive is vendored by sha256, as upstream's
own package does, although the platform installs `kamaji-etcd` 0.18.1
separately and sets `kamaji-etcd.deploy: false`. Apart from `Chart.yaml`
(version, `appVersion`, and the formatting `helm package` gives it) the files
are those of the commit.

`helm package` is not byte-reproducible: it stamps the current time on every
file, so two runs give archives with different sha256 (measured with Helm
3.21.4). For the two Kamaji charts the digest pinned in the BOM therefore means
"the one archive CI pushed", not "what anyone can rebuild bit for bit". The
script prints a second digest, `content`, over the path and sha256 of every file
in the archive; that one is equal across runs and is what decides "same
content" below. For the seven unchanged charts the published archive has the
sha256 of the upstream one.

Publishing never replaces a version. For each chart the script first pulls that
version from the mirror: the same files mean the chart is skipped, other files
fail the run, and an answer other than "not found" or "denied" fails the run.
After a push the chart is pulled back by the reported manifest digest and its
bytes are compared with the pushed archive.

Order of use:

1. Merge the pull request that adds or changes `charts-mirror.yaml`. Its dry
   run already fetched, verified and packaged every chart.
2. Tag a commit of `main` `charts-mirror-v<n>` (`charts-mirror-v1` the first
   time). The tag only selects the commit; the manifest decides what is
   published. A tag and not a push to `main`: a published version can never be
   replaced, so publishing is an owner step on a chosen commit, as for the
   other packages, and not a side effect of every merge.
3. Make each new package public on GHCR (owner step, package settings). A
   package pushed by the workflow starts private, and `bom-verify` pulls
   anonymously.
4. In a follow-up pull request, point the BOM's third-party charts at
   `ghcr.io/mutex-solutions/charts-mirror` with the versions of the manifest
   and the manifest digests of the run (the job summary and the
   `charts-mirror-digests-<sha>` artifact, `charts-mirror.json`). `bom-verify`
   checks them like our own OCI charts; it needed no change.
5. The release can then drop `-dev`.

A run that fails after some pushes can be run again (re-run the job, or a next
tag): what is already served with the same content is skipped and its digest
is printed again.

Licences of the mirrored charts (the licence file of each upstream repository,
read on 2026-11-16): Apache-2.0 for all nine, `argoproj/argo-helm` (argo-cd),
`cert-manager/cert-manager`, `cilium/cilium`, `metallb/metallb`,
`cloudnative-pg/charts`, `sergelogvinov/proxmox-csi-plugin`, `clastix/kamaji`
(kamaji, kamaji-crds) and `clastix/kamaji-etcd`. Apache-2.0 allows
redistribution, also of a modified work if the change is stated; the only
change is the version stamp of the two Kamaji charts, stated here and in the
manifest. The script refuses a manifest entry whose `licence` is not Apache-2.0
or MIT. Only the archives of cilium, cloudnative-pg and kamaji-etcd contain a
licence file (the kamaji archive holds the one of its vendored kamaji-etcd),
and cert-manager names Apache-2.0 in a `Chart.yaml` annotation; the others are
republished as upstream ships them, without a licence file.
This is not legal advice.

Not verified:

- Any push. `helm push`, the read-back, the skip of a served version and the
  workflow's publish job have only run against a fake `helm` in the unit tests.
  The dry run (fetch, verify, package, and the read of the still absent mirror)
  ran against the real registries.
- How GHCR answers a pull of a missing package for a logged-in workflow token.
  Anonymously it answers `denied`, which the script counts as absent; if the
  token gets another answer the run fails before any push and the pattern has
  to be extended.
- That "denied" can never hide a served version. It rests on GHCR giving no
  write access without read access; in that case the push is refused too.
- The check and the push are not atomic. The workflow's concurrency group runs
  one publication at a time; a push from outside this workflow is not excluded
  by code.
- Signatures and provenance of the upstream charts and of the Kamaji source.
  The pins are the digests the BOM already held and one git commit.
- The Kamaji charts on a cluster. They render with `helm template`; the lab has
  not installed them from the mirror.
- That ArgoCD pulls by digest. It still pulls by version; the mirror makes a
  version immutable by this workflow's rule, not by the registry.
- The Helm download itself beyond its size and sha256, which were taken from
  `get.helm.sh` on 2026-11-16.

## Current release-gate limitations

The workflows currently **do not** provide a single atomic release gate:

- Docker publication, `test.yml` and `security-scan.yml` are independent. Tag
  pushes run neither tests nor scans; the only check they run before the image
  builds is the platform BOM gate above. Choose an exact commit whose tests and
  security gate completed successfully.
- GitHub Release creation waits for all five image builds, but not for the
  separate Helm publication workflow (which runs the platform BOM gate itself). Verify both before telling users to install.
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
