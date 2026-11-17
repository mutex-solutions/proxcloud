# CI, template builds and release operations

This page describes the checked-in workflows, not a guarantee that a release has
already been published. As of **2026-11-17**, GHCR has public development images,
the `v0.4.0-rc.1` prerelease (tag 2026-11-11, images and app chart `0.4.0-rc.1`)
and the first stable release `v0.4.0` (tag 2026-11-12, images and app chart
`0.4.0`), then the stable `v0.5.0` (tag 2026-11-13), the patches `v0.5.1` and `v0.5.2` and the stable `v0.6.0` (tags 2026-11-14),
and the `v0.7.0-rc.1` prerelease and the stable `v0.7.0` (tags 2026-11-15). The next release is the stable
`v0.8.0`; pushing its tag runs the app image/chart publication and GitHub Release
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
| `charts-mirror.yml` | PRs to `main` that touch the mirror manifest, the BOM, the script or its tests; `charts-mirror-v*` tags | PR runs the script's tests and a dry run with a read-only token: every third-party chart of the BOM is fetched, verified against its pin and packaged, nothing is pushed. A tag on a commit of `main` pushes each chart version that [the chart mirror](#chart-mirror) definitely does not serve yet to `ghcr.io/mutex-solutions/charts-mirror`, does not push over a served version, and records the digests. That is a rule of the script, not of the registry; see [limits of the gate](#limits-of-the-gate). Hosted runner. |
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
chart `0.8.0`, appVersion/image tag and installer BOM `v0.8.0`, platform BOM `release: 0.8.0`, and the same tag as
`DEFAULT_VERSION` of the Docker Compose install (`deploy/compose/proxcloud-up.sh`).
Keep `images.build: true` until the exact chart and images are published and
verified; use `false` explicitly for released-artifact evaluation after that.
The installer's broker-free floor compares the SemVer **base** against `0.4.0`:
valid prereleases and build metadata are accepted syntactically, not verified as
published. App publication tags are narrower (stable/RC, no build metadata), since
images use the raw tag and Docker tags cannot contain `+`.

Managed charts use their own metadata versions and reject a mismatching tag.
`database-cluster` `0.5.1`, `proxmox-kamaji-cluster` `0.1.13` (placement by free capacity) and
`proxcloud-platform` `0.1.2` (read access to Machines), all three published from their own tags
and pinned by digest in the platform BOM, need separate
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

A `pending` digest has nothing to compare and is printed as `skip` (the BOM
holds none today). A mismatch, a
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

The release commit of `v0.8.0` sets the BOM's `release` to `0.8.0`. `0.8.0-dev`
was the only thing the gate still refused: no pin is `pending` and no version is
`0.0.0+` since the BOM takes its third-party charts from
[the chart mirror](#chart-mirror) (checked on 2026-11-17 on that commit:
`--release` prints `all pins match`). A commit whose BOM is `-dev` cannot be
tagged, as `vX.Y.Z` or as `vX.Y.Z-rc.N`. After the `v0.8.0` tag the BOM on `main`
was set to `0.9.0-dev`, so `main` cannot be tagged again until the next release
commit drops `-dev`. The gate does not compare the BOM's release with the tag
(below): a commit whose BOM has no `-dev` passes this part for any tag.

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
so that Kamaji has an exact chart version. By rule only the `charts-mirror.yml`
workflow publishes there; the registry does not enforce that rule (see
[limits of the gate](#limits-of-the-gate)). Tag `charts-mirror-v1` published all nine charts on 2026-11-16, the
packages are public, and the BOM takes every third-party chart from the mirror.

`deploy/platform/charts-mirror.yaml` lists every third-party chart of the BOM
with its source. A test (`test_charts_mirror.py`) keeps it equal to the BOM:
the same charts and versions, and no chart the BOM does not use. The manifest's
digests are those of the upstream sources; the BOM holds the mirror's manifest
digests, which the test does not compare (`bom-verify` checks them against the
registry).
[`charts_mirror.py`](../.github/scripts/charts_mirror.py) (standard library,
`helm` and `git`) handles each entry by its kind:

| Source | What is fetched and checked | What is published |
|---|---|---|
| `https://` repository | `helm pull` of the exact version; the sha256 of the `.tgz` must equal the pinned index digest. | The downloaded archive, unchanged. |
| OCI repository | `helm pull` by the pinned manifest digest. | The pulled archive, unchanged. |
| Upstream git tag (`kamaji`, `kamaji-crds`) | `git fetch` of the tag, which must still point at the pinned commit; no symlink in the chart directory; the chart's dependencies must be exactly the one the manifest pins by sha256, or none. | `helm package --version --app-version` of the chart directory, after `LICENSE` and `NOTICE` of the repository root were copied into it, one paragraph was added to `NOTICE` and one annotation to `Chart.yaml`. |

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
separately and sets `kamaji-etcd.deploy: false`.

Difference between a mirrored Kamaji chart and `charts/<chart>` of the upstream
commit, all of it:

- `Chart.yaml`: `version` and `appVersion` are `26.11.2-edge` (upstream:
  `0.0.0+latest` and `latest`); the annotation `charts-mirror/changes` is
  added (`Modified by oci://ghcr.io/mutex-solutions/charts-mirror: version set
  to 26.11.2-edge and appVersion set to 26.11.2-edge; see NOTICE`); and
  `helm package` rewrites the file (key order, no comments, long lines
  folded). Upstream's annotations are kept.
- `LICENSE`: added, a byte-for-byte copy of the `LICENSE` at the root of the
  upstream repository at that commit. The chart directories upstream hold
  none.
- `NOTICE`: added, the `NOTICE` at the root of the upstream repository at that
  commit, followed by one paragraph of ours that names the upstream
  repository, tag, commit and directory and lists these changes. The paragraph
  has no date, so it does not change the `content` digest between runs.
- `kamaji` only: `charts/kamaji-etcd/`, the dependency: the files of its
  publisher's 0.15.0 archive, unpacked by `helm package`, unchanged.
- Not included: `.helmignore`, `Makefile` and `README.md.gotmpl`, which the
  chart's own `.helmignore` excludes from any package.

The script fails when the repository has no `LICENSE`, when the chart directory
already holds a `LICENSE` or `NOTICE` (it does not overwrite upstream's), when
`Chart.yaml` does not have exactly one `annotations:` block or already has
that annotation, or when the packaged archive lacks either file or the
annotation. Checked on 2026-11-16 against commit `31fed7a654b3` with Helm
3.21.4: the root has both files (`LICENSE` 11344 bytes, `NOTICE` 913 bytes),
neither chart directory has one, and both packaged archives hold `LICENSE`
byte-equal to upstream's and a `NOTICE` that starts with upstream's 913 bytes.
`diff -r` of each upstream chart directory against the unpacked archive shows
exactly the differences listed above, and `charts/kamaji-etcd/` equals the
unpacked publisher archive (sha256 `b8e88d5f...f209`) file for file.

`helm package` is not byte-reproducible: it stamps the current time on every
file, so two runs give archives with different sha256 (measured with Helm
3.21.4). For the two Kamaji charts the digest pinned in the BOM therefore means
"the one archive CI pushed", not "what anyone can rebuild bit for bit". The
script prints a second digest, `content`, over the path and sha256 of every file
in the archive; that one is equal across runs and is what decides "same
content" below. For the seven unchanged charts the published archive has the
sha256 of the upstream one.

The script does not push over a version the mirror serves. For each chart it
first pulls that version from the mirror, and all nine are compared before the
first push:

- The same files: the chart is skipped.
- Other files: the run fails.
- A definite "not found": the version is absent and is pushed. That is one
  answer only, as the last line of Helm's output, for exactly this chart and
  version:
  `Error: failed to perform "FetchReference" on source: ghcr.io/mutex-solutions/charts-mirror/<chart>:<version>: not found`.
  Helm 3.21.4, the release the workflow pins, prints this for a missing
  version and for a missing repository (measured anonymously on 2026-11-16:
  a missing version on ghcr.io and quay.io, a missing repository on
  registry.k8s.io, public.ecr.aws and mcr.microsoft.com). The words "not
  found" elsewhere in an answer do not count: a 404 of the token endpoint, a
  missing credential helper, a layer that cannot be read and a proxy error
  all contain them.
- With `--push`, anything else fails the run before the first push. That
  includes `denied`, `unauthorized`, 401 and 403: GHCR answers `denied` for a
  package that does not exist (anonymously; measured) and, as far as we know,
  for one the caller may not read, and it moves a tag on push instead of
  refusing, so a refusal must not count as "absent".
- Without `--push` (pull requests and the dry run) the read is anonymous and
  nothing is pushed. A refusal is then reported per chart as "not compared"
  and does not fail the run; any other unknown answer still fails. Before the
  packages existed and were public, every dry run reported "not compared".

After a push the chart is pulled back by the reported manifest digest and its
bytes are compared with the pushed archive.

Order of use:

0. Owner, once, before the first tag: the repository settings under
   [limits of the gate](#limits-of-the-gate). The workflow runs without them;
   they are what limits who can publish.
1. Merge the pull request that adds or changes `charts-mirror.yaml`. Its dry
   run already fetched, verified and packaged every chart.
2. Tag a commit of `main` `charts-mirror-v<n>` (`charts-mirror-v1` the first
   time). The tag only selects the commit; the manifest decides what is
   published. A tag and not a push to `main`: a published version is not meant
   to change, so publishing is an owner step on a chosen commit, as for the
   other packages, and not a side effect of every merge.
3. Make each new package public on GHCR (owner step, package settings). A
   package pushed by the workflow starts private, and `bom-verify` pulls
   anonymously.
4. Done in the follow-up change of 2026-11-16, after `charts-mirror-v1`: the
   BOM takes all nine third-party charts from
   `ghcr.io/mutex-solutions/charts-mirror`, with the versions of the manifest
   and the manifest digests of the run (the job summary and the
   `charts-mirror-digests-<sha>` artifact, `charts-mirror.json`), each compared
   with an anonymous manifest `HEAD` before it was written. `bom-verify` checks
   them like our own OCI charts; `bom_verify.py` did not change. A next mirror
   tag (a new chart version) needs the same follow-up, and the same change must
   move what the tests tie to the BOM, or `test.yml` and `bom-verify.yml` fail:
   - `TestBOMMatchesStage40` (`internal/platformlife/bom_test.go`) requires
     `spec.chart` of `installer/playbooks/templates/argocd-helmchart.yml.j2`
     to be `oci://<BOM repo>/argo-cd`, and `repoURL` of the cert-manager
     Application in `installer/playbooks/templates/apps-operators.yml.j2` to
     be the BOM repo. Both templates now name the mirror. For cert-manager
     that turned the stage-40 Application from an `https://` Helm repository
     into an OCI one (`repoURL` without a scheme, `chart`, `targetRevision`),
     so the installer's management cluster needs the mirror packages to be
     public. `helm template` of both charts from the mirror with the values of
     the templates renders (Helm 3.21.4, anonymous); neither was run on a
     cluster.
   - `platformlife.Validate` (`internal/platformlife/bom.go`): `digest:
     pending` in a `-dev` BOM is for our own packages, the repositories under
     `ghcr.io/mutex-solutions/` that our own tags publish. The mirror is under
     that prefix and is excluded from it (`ownPackage`): its charts are
     third-party content that exists before the BOM names it, so a mirror
     chart without a digest is refused in every BOM, `-dev` included.
     `TestValidate` has both sides: `pending` on a mirror chart and on an
     upstream chart is refused, on one of our own charts in `-dev` it is
     accepted.
   - `TestValidate`, "a release keeps no pending digest and no 0.0.0+
     version": the BOM holds no `0.0.0+` version any more, so the case sets
     one.
   - `TestRenderGolden` (`internal/platformlife/render_test.go`):
     `testdata/render-seed-argocd.golden.yaml`, regenerated with `-update`.
     The renderers did not change: a bare repository was already rendered as
     an OCI source (`repoURL` without `oci://`, `chart`, `targetRevision`)
     with one ArgoCD repository entry (`enableOCI: "true"`) per repository.
     The project now allows two source repositories, the mirror and
     `ghcr.io/mutex-solutions/charts`, and has those two entries.
   - `test_the_repository_bom_parses`
     (`.github/scripts/tests/test_bom_verify.py`) required all three kinds of
     pin, `oci`, `index` and `url`. With every chart on OCI no `index` pin is
     left; it now requires `oci` and `url`, allows `index`, and requires every
     mirror chart to have a digest, no `0.0.0+` version and a valid OCI tag.
   - The `kamaji` component no longer sets `image.tag`: the mirrored chart's
     `appVersion` is `26.11.2-edge` and `helm template` gives the same output
     with and without the value (image `clastix/kamaji:26.11.2-edge`).
     `TestBOMMatchesInstallerVersions` now compares the installer's
     `kamaji_image` with the version of both Kamaji charts.
   - `deploy/platform/charts-mirror.yaml` did not change: it names the
     upstream sources, and its test compares name and version for a BOM chart
     that comes from the mirror.
   - Not tied to the BOM by a test, and still naming upstream:
     `installer/playbooks/files/services-platform.sh` (stage 50, Kamaji as
     chart `0.0.0+latest` with the image tag) and the add-on defaults in
     `deploy/charts/proxmox-kamaji-cluster/values.yaml`.
5. The release can then drop `-dev`.

The script writes `charts-mirror.md` and `charts-mirror.json` while it runs:
after every verified chart, before every push and after it. Until the run has
finished they are marked incomplete (`"complete": false`, an error text, and
per chart the result and the digest the run got). A run that fails after at
least one chart was verified, or that is killed (job timeout, cancel), so
leaves that record, and the job puts the table in its summary and in the
artifact. A chart whose push was started but not confirmed has the result
"push attempted, not confirmed" and no digest there; its digest, if the push
reached the registry, is in the error text, in the step log or in the next
run. When no record exists at all the summary says so and does not claim that
nothing was published. Tested with an exception raised inside a push (unit
test) and with `SIGKILL` of a local dry run after three verified charts; a
kill during a real push, and on a runner, was not tried. The run can be repeated
(re-run the job, or a next tag): what is served with the same content is
skipped and its digest is printed again.

### Limits of the gate

What the workflow and the script do:

- The publish job runs only for a `charts-mirror-v<n>` tag in this repository
  and checks that the tagged commit is an ancestor of `main`.
- The script pushes only a version that the mirror definitely does not serve,
  and fails closed on every other answer.

What they cannot do, and what this document therefore does not claim:

- On a tag push GitHub runs the workflow file of the tagged commit. A tag on a
  commit that is not on `main` can carry a workflow without the ancestor step,
  and another script or manifest. The step stops a mistake, not a person with
  write access. `clusterctl.yml` has the same limit.
- More generally, any workflow file in any branch or tag that a person with
  write access pushes can ask for `packages: write`. As far as we know GHCR
  grants package access per repository, not per workflow, branch or tag; that
  was not tested. A GitHub Environment on the publish job would not change
  this: the job uses the built-in token, not a secret held by an environment,
  and the tagged commit can drop the `environment:` line. None is configured.
- GHCR does not refuse a push to an existing tag; it moves the tag. "A version
  is never replaced" is a rule of this script, which the commit that runs it
  controls.
- The check and the push are two steps. The concurrency group serialises this
  workflow's own runs only.

Owner actions (repository and organisation settings, not code; none of them was
checked from this repository):

1. A tag ruleset for `charts-mirror-v*` that restricts creation, update and
   deletion to the owner. This narrows the intended path. It does not stop
   another workflow file from pushing to the package.
2. Write access to the repository limited to the people who may publish
   packages, and `main` protected (reviews, no force push). This is the control
   that limits who can publish. Pull requests from forks get a read-only token.
3. After the first run: in each mirror package's settings, Actions access for
   this repository only.

What notices a replaced version: the BOM pins every mirror chart by manifest
digest, so `bom-verify` (pull requests that touch the BOM, weekly, and the release
gate) fails when the version resolves to another digest. ArgoCD still pulls by
version, so between two checks it would install the replaced chart.

Licences of the mirrored charts (the `LICENSE` file at the head of each upstream
repository, read on 2026-11-16; for Kamaji also at the pinned commit). All are
Apache-2.0, which allows redistribution, also of a modified work when the
licence is given, the change is stated and a `NOTICE` is kept (section 4). The
script refuses a manifest entry whose `licence` is not Apache-2.0 or MIT; the
value is typed by a person, not detected.

| Chart | Upstream repository | Licence text in the mirrored archive |
|---|---|---|
| argo-cd | <https://github.com/argoproj/argo-helm> | None. Republished as upstream ships it. |
| cert-manager | <https://github.com/cert-manager/cert-manager> | None; `Chart.yaml` names Apache-2.0 in an annotation. Republished as upstream ships it. |
| cilium | <https://github.com/cilium/cilium> | `LICENSE`, from upstream. |
| metallb | <https://github.com/metallb/metallb> | None. Republished as upstream ships it. |
| cloudnative-pg | <https://github.com/cloudnative-pg/charts> | `LICENSE`, from upstream. |
| proxmox-csi-plugin | <https://github.com/sergelogvinov/proxmox-csi-plugin> | None. Republished as upstream ships it. |
| kamaji-etcd | <https://github.com/clastix/kamaji-etcd> | `LICENSE`, from upstream. |
| kamaji-crds | <https://github.com/clastix/kamaji> | `LICENSE` and `NOTICE`, added by the mirror from the pinned commit. |
| kamaji | <https://github.com/clastix/kamaji> | `LICENSE` and `NOTICE`, added by the mirror from the pinned commit. |

Each upstream repository has a `LICENSE` file at its root; the link above is
where a recipient of an archive without one finds the licence. Of the eight
repositories only `clastix/kamaji` has a `NOTICE` file.

Open, for the owner to decide:

- Four archives (argo-cd, cert-manager, metallb, proxmox-csi-plugin) are
  republished byte for byte without a licence file, because adding one would
  change the archive and its sha256 would no longer equal upstream's.
  Section 4(a) asks that recipients get a copy of the licence. Whether this
  table is enough, or whether these four should be repackaged with a `LICENSE`
  like the Kamaji charts (and then pinned by the mirror's digest only), is not
  decided.

Section 4(b) asks that a modified file carries a notice of the change. The one
modified file is `Chart.yaml`. `helm package` drops comments and keeps
annotations, so the notice is the annotation `charts-mirror/changes` in that
file; the chart's `NOTICE`, the manifest and this document state the change
too.

This is not legal advice.

Not verified:

- Any push. `helm push`, the read-back, the skip of a served version and the
  workflow's publish job have only run against a fake `helm` in the unit tests.
  The dry run (fetch, verify, package, and the read of the still absent mirror)
  ran against the real registries.
- How GHCR answers a pull of a missing package for a logged-in workflow token.
  Anonymously it answers `denied` (403 from the token endpoint, measured on
  2026-11-16). The publish run needs a definite "not found". If the logged-in
  token also gets `denied` for a package that does not exist yet, the first
  publish run fails before any push; nothing is published and the script then
  needs another authoritative read (for example the registry's tag list with
  that token). This is the most likely reason for a first run to fail.
- That GHCR never answers "not found" for a version it serves but the token
  may not read. In that case the script would push; whether GHCR then accepts
  the push is not known.
- Helm's answer for a missing version of a mirror package that exists, with
  the workflow's token. The accepted line was measured anonymously against
  other public packages (see above), not against the mirror, which has no
  package yet. If GHCR or a later Helm release words it differently, the
  publish run fails before the first push.
- The owner settings under [limits of the gate](#limits-of-the-gate), and the
  statement there that package access is per repository.
- Signatures and provenance of the upstream charts and of the Kamaji source.
  The pins are the digests the BOM already held and one git commit.
- The Kamaji charts on a cluster. They render with `helm template`; the lab has
  not installed them from the mirror.
- That ArgoCD pulls by digest. It still pulls by version; a version stays the
  same by this workflow's rule, not by the registry.
- The workflow file on GitHub. It parses as YAML and its unit test passes;
  `actionlint` was not run and no run happened.
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
