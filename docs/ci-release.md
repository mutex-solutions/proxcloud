# CI, template builds and release operations

This page describes the checked-in workflows, not a guarantee that a release has
already been published. As of **2026-11-21**, GHCR has public development images,
the `v0.4.0-rc.1` prerelease (tag 2026-11-11, images and app chart `0.4.0-rc.1`)
and the first stable release `v0.4.0` (tag 2026-11-12, images and app chart
`0.4.0`), then the stable `v0.5.0` (tag 2026-11-13), the patches `v0.5.1` and `v0.5.2` and the stable `v0.6.0` (tags 2026-11-14),
the `v0.7.0-rc.1` prerelease and the stable `v0.7.0` (tags 2026-11-15), the stable `v0.8.0` (tag 2026-11-17) and the patches `v0.8.1` and `v0.8.2` (tags 2026-11-18), and the release candidates
`v0.9.0-rc.1` and `v0.9.0-rc.2`, prereleases, the stable `v0.9.0` and the patch `v0.9.1` (tags 2026-11-19; [Release candidates](#release-candidates)), and the first candidate of `v0.10.0`, `v0.10.0-rc.1`, a prerelease, the stable `v0.10.0` and the two candidates of `v0.10.1`, `v0.10.1-rc.1` and `v0.10.1-rc.2`, prereleases (tags 2026-11-20), and the stable `v0.10.1` (tag 2026-11-21). Pushing the next tag runs the app image/chart publication and GitHub Release
creation.
This source change does none of those operations, and installation from released
artifacts and later controlled PVE template canaries remain separate steps. Read
[release readiness](release-readiness.md) before publication or evaluation.

## Workflow map

| Workflow | Triggers | Effect / runner |
|---|---|---|
| `test.yml` | PRs targeting `main` (only the jobs the changed files can affect: [What runs when](#what-runs-when)); pushes to `main` | Hosted Go unit/integration tests, both SPA build/smoke tests, installer syntax/tests, credential-free Packer validation and offline image-pipeline tests, naming gates. Not triggered by release tags. |
| `security-scan.yml` | PRs targeting `main` (only the scans the changed files can affect: [What runs when](#what-runs-when)); pushes to `main`; daily schedule; manual dispatch | Exact-head Trivy 0.76.2 scans of lockfiles, six local product images, eleven non-GPU template outputs, thirteen raw upstream bases (report-only) and a nondefault controlled-base prototype; required `security-gate` job and a PR summary comment. Nothing is pushed. Not triggered by release tags. |
| `docker.yml` | PRs to `main`, `main` pushes, `v*` tags, manual dispatch | Job `bom-verify` runs first; on a tag it is the [platform BOM release gate](#platform-bom-verification-and-release-gate) and the builds wait for it. PRs build, without pushing, those of the five images whose sources they change ([What runs when](#what-runs-when)). Image-relevant `main` pushes publish `dev-<7-char-sha>`; docs/chart-only changes can skip. Tags publish exact image tags; manual dispatch pushes `dev-<sha>` or `dev-<suffix>`. Hosted runner. |
| `helm.yml` | Chart-path PRs to `main`; app or managed-chart tags | PR lint/render only. App `v*` publishes `proxcloud`; `database-cluster-v*`, `proxmox-kamaji-cluster-v*` and `proxcloud-platform-v*` publish their independent charts. Hosted runner; no publication on ordinary merge. |
| `charts-mirror.yml` | PRs to `main` that touch the mirror manifest, the BOM, the script or its tests; `charts-mirror-v*` tags | PR runs the script's tests and a dry run with a read-only token: every third-party chart of the BOM is fetched, verified against its pin and packaged, nothing is pushed. A tag on a commit of `main` pushes each chart version that [the chart mirror](#chart-mirror) definitely does not serve yet to `ghcr.io/mutex-solutions/charts-mirror`, does not push over a served version, and records the digests. That is a rule of the script, not of the registry; see [limits of the gate](#limits-of-the-gate). Hosted runner. |
| `bom-verify.yml` | PRs to `main` that touch `deploy/platform/**` or the script; weekly schedule; manual dispatch | Compares every pin of the platform BOM, and of each [previous BOM](#previous-boms), with its registry by anonymous pull. Read-only token, no secret, nothing is pushed. Hosted runner. |
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

## What runs when

A pull request runs the jobs its changed files can affect. A push to `main`, a tag, the schedule
and a manual run are unchanged: they run everything (for `docker.yml` on `main`, everything the
existing image-source filter of its `changes` job lets through).

Each of `test.yml`, `docker.yml` and `security-scan.yml` starts with a job `changes`. It runs
`.github/scripts/ci_changes.py detect`, which lists the files between the point where the pull
request left its base branch and its head (`git diff --name-only --no-renames`, so a moved file
counts under both names and a deleted file under its own) and matches them against the table
`RULES` of that script. The table below is that table.

| Area (changed files) | `test.yml` | `docker.yml` (build, no push) | `security-scan.yml` |
|---|---|---|---|
| Documentation: `docs/**/*.md`, root `*.md`, `LICENSE` | `rebrand-gate` | none | none |
| Documentation a test reads: `CHANGELOG.md` | `go-test`, `installer`, `rebrand-gate` | none | none |
| `docs/releases/**` | `installer`, `rebrand-gate` | none | none |
| `docs/proxmox-permissions.md`, `docs/vm-artifact-publishing.md`, `docs/billing.md` | `go-test`, `rebrand-gate` | none | none |
| Go: `cmd/**`, `internal/**`, `pkg/**` | `go-test`, `installer` | api, sandbox-proxy, sandbox-runtime | filesystem, api, sandbox-proxy, sandbox-runtime (and sandbox-import for `cmd/sandbox-import/**`, `internal/sandboximport/**`; `template-images` for `cmd/mcp-gateway/**`) |
| `go.mod`, `go.sum` | `go-test`, `installer` | api, sandbox-proxy, sandbox-runtime | filesystem, the four Go images, `template-images` |
| `test/**` | `go-test`, `installer` | none | filesystem |
| `deploy/**` (charts, Compose, cluster manifests, clusterctl, ...) | `go-test`, `installer` | none | filesystem |
| of which `deploy/seed/**`, `deploy/platform/**`, `deploy/embed.go`, the two `argocd-health.yaml`, `deploy/Dockerfile` (and a `deploy/Dockerfile.dockerignore`) | the same | api | also api (`base-images` for the Dockerfile) |
| of which `deploy/sandbox-*.Dockerfile` | the same | that image | also that image and `base-images` |
| of which `deploy/templates/**` except `images.yaml` | the same | none | also `template-images`, controlled-base-prototype, `base-images` |
| of which `deploy/templates/images.yaml` | the same | none | also `base-images` |
| `installer/**` | `go-test`, `installer` | none | filesystem |
| `images/**` | `installer` | none | filesystem |
| `web/client/**` (its bundled Markdown under `src/docs/content` included) | `spa-smoke (client)` | web | filesystem, web-client |
| `web/admin/**` (the same) | `spa-smoke (admin)` | admin | filesystem, web-admin |
| other `web/**` (`Dockerfile`, `nginx.conf`, entrypoint, `web/tests`) | `installer` | web, admin | filesystem, web-client, web-admin (`base-images` for the Dockerfile) |
| `.github/**` (workflows, scripts, their tests) | everything | everything | everything |
| anything else (root `Makefile`, `.dockerignore`, `docker-compose.yml`, a new top-level directory, a file under `docs/` that is not Markdown) | everything | everything | everything |

Rules behind the table:

- **Unknown means everything.** A file that matches no row, an empty diff, a diff that cannot be
  computed, and any event that is not a pull request run every job. The output `reason` of
  `changes` says which of these applied.
- **Under `docs/` only Markdown is documentation.** `go test ./...` compiles a `.go` file there
  and the filesystem scan reads a lockfile there, so any other file type under `docs/` is unknown.
- **A new path in no row also turns one test red.** `test_ci_changes.py` keeps a reviewed list of
  the tracked paths that are in no row. A pull request that adds such a path (`.editorconfig`, a
  new top-level directory) runs every job, and the `installer` job of `test.yml` (not a required
  check) fails on that test until the path has a row in `RULES` or is on the list. The `changes`
  job of `security-scan.yml` skips this one test, so the required check is not held by it.
- **The image rows follow the Dockerfiles.** `test_ci_changes.py` reads the `COPY` sources of
  every scanned Dockerfile and fails, in `changes` of `security-scan.yml` too, when one of them
  does not select its image. It also fails on a tracked symbolic link, which would show a file
  under a path the diff does not list.
- **Tests read across directories**, so the test jobs are wider than the image jobs: Go tests
  render the charts under `deploy/`, read installer playbooks and three documents; the script
  tests of the `installer` job read Go sources, Dockerfiles, workflows, `CHANGELOG.md` and
  `docs/releases/`. A test that starts reading another document must add it to `RULES`:
  `test_ci_changes.py` finds such a read in the Go tests and fails until the row exists.
- **`rebrand-gate` always runs**: it reads every file, documentation included, in about 10 seconds.
  `bom-verify` of `docker.yml` always runs too (off a tag: its unit tests, 5 seconds).
- **`template-images`** builds and scans the eleven sandbox templates in about 15 minutes. On a
  pull request it runs for `deploy/templates/**` (not `images.yaml`, which only holds published
  digests), `cmd/mcp-gateway/**`, `go.mod`/`go.sum` (the gateway template compiles with them)
  and `.github/**` (the scanner, its policy, the Trivy pin and the workflow live there).

To see what a branch would run: `git diff --name-only --no-renames origin/main...HEAD | python3 .github/scripts/ci_changes.py classify`.

### The required check when jobs are skipped

The ruleset `require-security-gate` requires one check, `security-gate`, on a branch that is up
to date with `main`. The job runs on every pull request (`if: always()`), so the check is never
left waiting. It hands its whole `needs` context to `ci_changes.py gate`:

| State of a needed job | Verdict |
|---|---|
| `success` | passes |
| `failure` or `cancelled` | blocks, whatever `changes` said |
| `skipped`, and `changes` succeeded and did not ask for the job | passes as **not applicable**; the job summary and the pull request comment name every target that was not scanned |
| `skipped`, but `changes` asked for the job | blocks |
| `changes` itself failed, was cancelled, or gave no output | blocks: no scan runs and no skip is trusted |
| any state, and `changes` has no readable output for that job (`true`, `false` or a JSON list) | blocks: a job added to `needs` must get an output of `changes` |

A documentation-only pull request therefore gets a green `security-gate` whose summary says that
nothing was scanned, and the comment `security-summary` writes shows `Coverage: 0/32 targets ·
32 not scanned in this run`. `test.yml` has no aggregate job and no required check; when its
`changes` job fails, its three gated jobs run.

### What still scans everything

Before this split, every pull request rescanned all 32 targets with that hour's vulnerability
database, so a new CVE in a sandbox template showed up on the next pull request of anyone. Now:

- every push to `main` runs the full `security-scan.yml` (unchanged);
- `security-scan.yml` runs in full **every day at 02:23 UTC** on `main` (`schedule`) and on
  demand (`workflow_dispatch`). This run is the one that finds a new CVE in a target nobody
  touched.
- a full scan of `main` (push, schedule or manual run) that fails opens the issue
  `security-scan: the full scan of main fails` with a link to the run, or adds a comment to it
  while it is open (job `full-scan-issue`, the only job with `issues: write`; it runs no
  repository file). A person closes the issue. **It blocks nothing**: no pull request and no
  `v*` tag waits for a green full scan. `docker.yml` does not read the scan result on a tag,
  and `security-scan.yml` has never run on tags.
- a full scan of `main` is not cancelled by the next merge: the next one waits for it. Pull
  request runs still cancel their older run.
- `sandbox-templates.yml` rebuilds and republishes the templates weekly, but it does not scan
  them; and `security-scan.yml` scans images built from the source at that commit, not the
  digests published in `deploy/templates/images.yaml`.

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
chart `0.10.1`, appVersion/image tag and installer BOM `v0.10.1`, platform BOM `release: 0.10.1`, and the same tag as
`DEFAULT_VERSION` of the Docker Compose install (`deploy/compose/proxcloud-up.sh`).
Keep `images.build: true` until the exact chart and images are published and
verified; use `false` explicitly for released-artifact evaluation after that.
The installer's broker-free floor compares the SemVer **base** against `0.4.0`:
valid prereleases and build metadata are accepted syntactically, not verified as
published. App publication tags are narrower (stable/RC, no build metadata), since
images use the raw tag and Docker tags cannot contain `+`.

Managed charts use their own metadata versions and reject a mismatching tag.
`database-cluster` `0.5.2` (no disruption budget for a database of one instance),
`proxmox-kamaji-cluster` `0.1.16` (the node deletion timeout of the control plane's and the node
groups' machines) and `proxcloud-platform` `0.1.3` (replacing a Machine), all three published from their own tags
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
  On a tag the job also checks each [previous BOM](#previous-boms).
- `helm.yml`, job `publish`, on every `v*` tag with `--release`, as a step
  before the registry login. A published chart version is never replaced, so a
  tag that fails the gate must not publish the `proxcloud` chart either.

The release commit of `v0.8.0` sets the BOM's `release` to `0.8.0`. `0.8.0-dev`
was the only thing the gate still refused: no pin is `pending` and no version is
`0.0.0+` since the BOM takes its third-party charts from
[the chart mirror](#chart-mirror) (checked on 2026-11-17 on that commit:
`--release` prints `all pins match`). A commit whose BOM is `-dev` cannot be
tagged, as `vX.Y.Z` or as `vX.Y.Z-rc.N`. After the `v0.8.0` tag the BOM on `main`
was set to `0.9.0-dev`, so `main` could not be tagged again until the next release
commit dropped `-dev`: the release commit of `v0.8.1` sets it to `0.8.1` (checked on
2026-11-18 on that commit: `--release` prints `all pins match`, on the BOM and on the
previous BOM `0.8.0`). The pull request after that release commit set the BOM on
`main` to `0.9.0-dev` again and added `deploy/platform/previous/0.8.1.yaml`
([Previous BOMs](#previous-boms)), so `main` could not be tagged until the next release
commit dropped `-dev`: the release commit of `v0.8.2` sets it to `0.8.2` (checked on
2026-11-18 on that commit: `--release` prints `all pins match`, on the BOM and on the
previous BOMs `0.8.0` and `0.8.1`). The pull request after that release commit set the
BOM on `main` to `0.9.0-dev` again and added `deploy/platform/previous/0.8.2.yaml`, so
`main` could not be tagged until the next release commit dropped `-dev`: the commit of the
candidate `v0.9.0-rc.1` sets the release to `0.9.0-rc.1` (checked on 2026-11-19 on that
commit: `--release` prints `all pins match`, on the BOM and on the previous BOMs `0.8.0`,
`0.8.1` and `0.8.2`). The pull request after that tag set the BOM on `main` to
`0.9.0-dev` again and added `deploy/platform/previous/0.9.0-rc.1.yaml`, so `main` could not
be tagged until the next release commit dropped `-dev`: the commit of the candidate
`v0.9.0-rc.2` sets the release to `0.9.0-rc.2` (checked on 2026-11-19 on that commit:
`--release` prints `all pins match`, on the BOM and on the previous BOMs `0.8.0`, `0.8.1`,
`0.8.2` and `0.9.0-rc.1`). The pull request after that tag set the BOM on `main` to
`0.9.0-dev` again and added `deploy/platform/previous/0.9.0-rc.2.yaml`, so `main` could not
be tagged until the next release commit dropped `-dev`: the release commit of `v0.9.0` sets
the release to `0.9.0` (checked on 2026-11-19 on that commit: `--release` prints
`all pins match`, on the BOM and on the five previous BOMs). The pull request after that
tag set the BOM on `main` to `0.10.0-dev` and added
`deploy/platform/previous/0.9.0.yaml`, so `main` could not be tagged until the next release
commit dropped `-dev`: the release commit of `v0.9.1` sets the release to `0.9.1` (checked on
2026-11-19 on that commit: `--release` prints `all pins match`, on the BOM and on the six
previous BOMs). The pull request after that tag set the BOM on `main` to `0.10.0-dev` again
and added `deploy/platform/previous/0.9.1.yaml`, so `main` could not be tagged until the next
release commit dropped `-dev`: the commit of the candidate `v0.10.0-rc.1` sets the release to
`0.10.0-rc.1` (checked on 2026-11-20 on that commit: `--release` prints `all pins match`, on
the BOM and on the seven previous BOMs). The pull request after that tag set the BOM on `main`
to `0.10.0-dev` again and added `deploy/platform/previous/0.10.0-rc.1.yaml`, so `main` could not
be tagged until the next release commit dropped `-dev`: the release commit of `v0.10.0` sets the
release to `0.10.0` (checked on 2026-11-20 on that commit: `--release` prints `all pins match`, on
the BOM and on the eight previous BOMs). The pull request after that tag set the BOM on `main`
to `0.11.0-dev` and added `deploy/platform/previous/0.10.0.yaml`, so `main` could not be tagged
until the next release commit dropped `-dev`: the commit of the candidate `v0.10.1-rc.1` sets the
release to `0.10.1-rc.1` (checked on 2026-11-20 on that commit: `--release` prints `all pins match`,
on the BOM and on the nine previous BOMs). The pull request after that tag set the BOM on `main`
to `0.11.0-dev` again and added `deploy/platform/previous/0.10.1-rc.1.yaml`, so `main` could not be
tagged until the next release commit dropped `-dev`: the commit of the second candidate
`v0.10.1-rc.2` sets the release to `0.10.1-rc.2` (checked on 2026-11-20 on that commit: `--release`
prints `all pins match`, on the BOM and on the ten previous BOMs). The pull request after that tag
set the BOM on `main` to `0.11.0-dev` again and added `deploy/platform/previous/0.10.1-rc.2.yaml`,
so `main` could not be tagged until the next release commit dropped `-dev`: the release commit of
`v0.10.1` sets the release to `0.10.1` (checked on 2026-11-21 on that commit: `--release` prints
`all pins match`, on the BOM and on the eleven previous BOMs). The pull request after that tag
set the BOM on `main` to `0.11.0-dev` again and added `deploy/platform/previous/0.10.1.yaml`, so
`main` cannot be tagged until the next release commit drops `-dev`. The gate does not
compare the BOM's release with the tag
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

## Release candidates

A candidate `vX.Y.Z-rc.N` is a prerelease for a lab, not the stable release. Its first
use is the upgrade between BOMs, which takes only a release BOM as its target: a build of
`main` cannot drill it ([Upgrade drill builds](#upgrade-drill-builds)). What its commit
changes, and what it leaves:

| Item | At a candidate | Why |
|---|---|---|
| `release:` of the platform BOM | `X.Y.Z-rc.N` | A `-dev` BOM fails the release gate and is refused as an upgrade target. `-rc.N` is a release to both: `Validate` and `bom_verify.py` ask only whether the release ends in `-dev`, and a release takes no `pending` digest and no `0.0.0+` version |
| Renderer goldens | Regenerated (`-update`) | They carry the release label. The goldens of the previous BOMs do not change |
| App chart `version` and `appVersion`, `pc_versions.proxcloud`, `DEFAULT_VERSION` | Stay on the stable release | `test_chart_images_and_installer_pin_exact_same_stable_release` wants a pin whose policy is `--latest`. The workflows take the image tag, the chart version and the appVersion from the tag, not from these files |
| "Current stable release" in README, SECURITY and the guides | Unchanged | A candidate is not the stable release |
| `CHANGELOG.md` | `[Unreleased]` stays; a short `## [X.Y.Z-rc.N]` section is added | The release turns `[Unreleased]` into its section. `test_release_notes.py` wants a changelog section for every file under `docs/releases/` |
| `docs/releases/vX.Y.Z-rc.N.md` | Added, short | It is the body of the prerelease. Without it the body is the changelog section |

The tag publishes the five images as `vX.Y.Z-rc.N` and the app chart as `X.Y.Z-rc.N`, and
creates the GitHub Release with `--prerelease --latest=false`
(`GITHUB_REF_NAME=vX.Y.Z-rc.N python3 .github/scripts/release_policy.py` prints the flags).

After the tag, the pull request that sets the BOM on `main` back to `-dev` keeps the
candidate's BOM as a previous BOM, exactly as after a release
([Previous BOMs](#previous-boms), with `X.Y.Z-rc.N` as the name). A platform that a lab
moved to the candidate's BOM is applied by later builds only while they carry that file;
from it to the BOM of the release is one more upgrade run. `Previous` takes any name
whose BOM validates as a release, and `TestEveryReleaseHasItsPreviousBOM` reads
`## [X.Y.Z-rc.N]` sections too, so the pull request fails its tests without the file.

`v0.9.0-rc.1` is the first candidate cut this way (`v0.4.0-rc.1` and `v0.7.0-rc.1` had no
platform BOM gate, no previous BOM and no release notes file). Its tag is on the commit
`eee7b3dc`; the `docker` and `helm` runs of the tag succeeded and the GitHub Release is a
prerelease, with `v0.8.2` still the latest (read on 2026-11-19). The pull request after
it added `deploy/platform/previous/0.9.0-rc.1.yaml` ([Previous BOMs](#previous-boms)).
A lab moved a platform from the BOM `0.8.0` to that of the candidate with its published
images on 2026-11-19 ([managed-services.md](managed-services.md#upgrade-between-boms)).

`v0.9.0-rc.2` is the second: the same pins, the `release` line alone differs, and its
commit holds the three fixes for what that lab round found. Neither chart of the product
changed since its tag (`git diff proxmox-kamaji-cluster-v0.1.14 -- deploy/charts/proxmox-kamaji-cluster`
and `git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print nothing),
so the BOM needs no new chart version. Its tag is on the commit `8906fa93`; the `docker`
and `helm` runs of the tag succeeded and the GitHub Release is a prerelease, with `v0.8.2`
still the latest (read on 2026-11-19). The pull request after it added
`deploy/platform/previous/0.9.0-rc.2.yaml`. A lab installed a platform from the published
images of `v0.9.0-rc.2` on 2026-11-19, in bridge mode, and removed one
([managed-services.md](managed-services.md#not-verified-on-a-lab-yet)).

`v0.9.0` is the release the two candidates were cut for: the same pins once more, and its
BOM differs from that of `0.9.0-rc.2` in the `release` line alone. Neither chart of the
product changed since its tag (the two `git diff` commands above print nothing on the
release commit). The app chart, the installer pin and `DEFAULT_VERSION` move with it, as
at every stable release. Its tag is on the commit `ed82439a`; the `docker` run of the tag
succeeded, the `helm` run failed once at `helm push` on a registry error and succeeded on
a rerun of the failed job, and the GitHub Release is the latest (read on 2026-11-19). The
pull request after it added `deploy/platform/previous/0.9.0.yaml` and set the BOM on
`main` to `0.10.0-dev`.

`v0.9.1` is a patch on top of it, cut from `main`: its release commit takes the BOM from
`0.10.0-dev` to `0.9.1`, with the pins of `0.9.0` and the `release` line alone different.
Neither chart of the product changed since its tag (the two `git diff` commands above
print nothing on the release commit), so the BOM needs no new chart version. The app chart
did change (the CloudNativePG image name) and is published as `0.9.1` by the release tag,
as at every stable release. Its tag is on the commit `5ceca5e0`; the `docker` and `helm`
runs of the tag succeeded on the first attempt, and the GitHub Release is the latest (read
on 2026-11-19). The pull request after it added `deploy/platform/previous/0.9.1.yaml` and
set the BOM on `main` back to `0.10.0-dev`.

`v0.10.0-rc.1` is the first candidate of `v0.10.0`, cut from `main`: its commit takes the BOM
from `0.10.0-dev` to `0.10.0-rc.1`, with the pins `main` had (cluster chart 0.1.15, database
chart 0.5.2, platform chart 0.1.3). No chart of the product changed since its tag
(`git diff proxmox-kamaji-cluster-v0.1.15 -- deploy/charts/proxmox-kamaji-cluster`,
`git diff database-cluster-v0.5.2 -- deploy/charts/database-cluster` and
`git diff proxcloud-platform-v0.1.3 -- deploy/charts/proxcloud-platform` print nothing on its
commit), so the BOM needs no new chart version. The BOM names the k3s, the Kubernetes version
and the node image of `0.9.1`: no upgrade to it replaces the seed's k3s or a machine of the
platform cluster. The app chart, the installer pin and `DEFAULT_VERSION` stay on `v0.9.1`. Its
tag is on the commit `12492cb2`; the `docker` and `helm` runs of the tag succeeded on the first
attempt, and the GitHub Release is a prerelease, with `v0.9.1` still the latest (read on
2026-11-20). The pull request after it added `deploy/platform/previous/0.10.0-rc.1.yaml` and set
the BOM on `main` back to `0.10.0-dev`. The lab round of 2026-11-20 ran on its published images
([managed-services.md](managed-services.md#upgrade-between-boms)).

`v0.10.0` is the release that candidate prepared, cut from `main`: its release commit takes the
BOM from `0.10.0-dev` to `0.10.0`, with the pins of `0.10.0-rc.1` and the `release` line alone
different. No chart of the product changed since its tag (the three `git diff` commands above
print nothing on the release commit), so the BOM needs no new chart version. The BOM still names
k3s `v1.35.10+k3s1` (no newer patch of 1.35 existed on 2026-11-20), Kubernetes 1.35.10 and the
node image of `0.9.1`: no upgrade to it replaces the seed's k3s or a machine of the platform
cluster. The app chart, the installer pin and `DEFAULT_VERSION` move to `0.10.0` / `v0.10.0`. The
code differs from the candidate's in one change, the fixes for what the lab round found (the
`seed-backup` row, the withdraw warning, the audit row of a Delete that deleted nothing), which
has not run on a lab. Its tag is on the commit `8797f8ac`; the `docker` and `helm` runs of the tag
succeeded on the first attempt, and the GitHub Release is the latest (read on 2026-11-20). The
pull request after it added `deploy/platform/previous/0.10.0.yaml` and set the BOM on `main` to
`0.11.0-dev`.

`v0.10.1-rc.1` is the first candidate of `v0.10.1`, cut from `main` as the patch `v0.9.1` was: its
commit takes the BOM from `0.11.0-dev` to `0.10.1-rc.1`, with the pins `main` had. It is the first
release BOM that names another k3s and another Kubernetes than the ones before it: k3s
`v1.36.6+k3s1`, Kubernetes 1.36.6 and its node image, one minor up from every previous BOM, so
every upgrade to it replaces the seed's k3s and every machine of the platform cluster, in two
parts ([managed-services.md](managed-services.md#upgrade-to-kubernetes-1366)). No chart of the
product changed since its tag (the three `git diff` commands above print nothing on its commit),
so the BOM needs no new chart version, and no third-party chart or provider moves: cert-manager
v1.20.2 and Cilium 1.18.16 are then outside the Kubernetes versions their own documents list,
accepted as "to be proven by the lab drill". The app chart, the installer pin and
`DEFAULT_VERSION` stay on `v0.10.0`, and the installer's k3s and Kubernetes pins stay at 1.35.10.
Its tag is on the commit `dd14e17c`; the `docker` and `helm` runs of the tag succeeded on the first
attempt, and the GitHub Release is a prerelease while the latest stays `v0.10.0` (read on
2026-11-20). The pull request after it added `deploy/platform/previous/0.10.1-rc.1.yaml` and set
the BOM on `main` back to `0.11.0-dev`, with the pins of the candidate. Its drill ran on one lab
on 2026-11-20: part "seed" to the end, with a tenant cluster and a database serving, and part
"platform" as far as the control plane's unit, where it stopped (the old control plane machine's
Node object stayed; [managed-services.md](managed-services.md#upgrade-to-kubernetes-1366)).

`v0.10.1-rc.2` is the second candidate of `v0.10.1`, cut from `main` after the corrections of that
drill and after the tag `proxmox-kamaji-cluster-v0.1.16` had published the corrected cluster chart
(`sha256:2a58fa81b5a96fd7b23c76b06418af5a477586f151ec8e9eab1bad1536e6f000`, read from the registry manifest and by
`helm pull`). Its commit takes the BOM from `0.11.0-dev` to `0.10.1-rc.2` and pins that chart for
`pc-platform` and for `tenantCharts.kubernetes`; `pc-platform` lists 0.1.13, 0.1.14 and 0.1.15 in
`upgradeFrom`, so every previous BOM reaches it. Nothing else of the BOM moves, and the three
`git diff` commands, against `proxmox-kamaji-cluster-v0.1.16`, `database-cluster-v0.5.2` and
`proxcloud-platform-v0.1.3`, print nothing on its commit. Its Kubernetes pins are those of
`0.10.1-rc.1`, so an installation whose machines are being replaced toward `0.10.1-rc.1` goes on
under it with a Retry (`Upgrade.Carried`). The app chart, the installer pin and `DEFAULT_VERSION`
stay on `v0.10.0`. Its tag is on the commit `ef44cdaf`; the `docker` and `helm` runs of the tag
succeeded on the first attempt, and the GitHub Release is a prerelease while the latest stays
`v0.10.0` (read on 2026-11-21). The pull request after it added
`deploy/platform/previous/0.10.1-rc.2.yaml` and set the BOM on `main` back to `0.11.0-dev`, with
the pins of the candidate. Its drill ran on the same lab on 2026-11-20 and 21, on its published
images: one Retry took the platform the first drill had left on, and the units `system`, `kamaji`
and `dbaas` were replaced ([managed-services.md](managed-services.md#upgrade-to-kubernetes-1366)).

`v0.10.1` is the release those two candidates prepared, cut from `main`: its release commit takes
the BOM from `0.11.0-dev` to `0.10.1`, with the pins of `0.10.1-rc.2` and the `release` line alone
different. No chart of the product changed since its tag (`git diff` against
`proxmox-kamaji-cluster-v0.1.16`, `database-cluster-v0.5.2` and `proxcloud-platform-v0.1.3` prints
nothing on the release commit), so the BOM needs no new chart version. It names k3s
`v1.36.6+k3s1`, Kubernetes 1.36.6 and its node image, cluster chart 0.1.16 and the third-party
charts of `0.10.0`: cert-manager v1.20.2 and Cilium 1.18.16 run outside the Kubernetes versions
their own documents list, with no fault seen in the two drills. The app chart, the installer pin and
`DEFAULT_VERSION` move to `0.10.1` / `v0.10.1`; the installer's k3s and Kubernetes pins stay at
1.35.10. The code differs from that of `v0.10.1-rc.2` in one change, the fixes for what the second
drill found (the wait for the tenants between two units, the item and audit entry of a deleted
Node, the ACL rows of a released token, the reason of a database that waits for room), which has
not run on a lab. Its tag is on the commit `7cb4bee0`; the `docker` and `helm` runs of the tag
succeeded on the first attempt, and the GitHub Release is the latest (read on 2026-11-21). The
pull request after it added `deploy/platform/previous/0.10.1.yaml` and set the BOM on `main` back
to `0.11.0-dev`, with the pins of the release.

Not verified on a lab: anything on the published images of `v0.9.0` or `v0.9.1`, the move
of a platform from a candidate's BOM to that of `0.9.0`, and any move to that of `0.9.1`.
One production site moved its platform from the BOM of 0.8.0 to that of `0.9.0` on the
published images of `v0.9.0`
([managed-services.md](managed-services.md#upgrade-between-boms)). Not verified either:
anything on the published images of `v0.10.1`, any move to the BOM `0.10.1`, and the move to
Kubernetes 1.36.6 from a BOM other than `0.10.0`.

## Previous BOMs

A platform records the BOM that installed it by the sha256 of the BOM file, and
an apply on it runs with that BOM. A release therefore carries, next to its own
BOM, the BOM of each earlier release back to `0.8.0`, as
`deploy/platform/previous/<release>.yaml` (`deploy/embed.go`,
`platformlife.Previous`). Without the file of a release, the next release
refuses every apply on the platforms that release installed
(`bom_unsupported`). A release's own `release:` line makes its BOM another file
than the one before it, so this holds for a patch release that changes no pin
too.

After each release tag `vX.Y.Z` (and each candidate tag, [Release candidates](#release-candidates)), in the pull request that sets the BOM on `main`
back to `-dev`:

1. Copy the BOM of the tag, byte for byte:
   `git show vX.Y.Z:deploy/platform/platform-bom.yaml > deploy/platform/previous/X.Y.Z.yaml`.
   Do not edit it afterwards, comments included: the hash is the key.
2. Add its hash (`sha256sum deploy/platform/previous/X.Y.Z.yaml`) to
   `previousSHA256` in `internal/platformlife/bom_test.go`.
3. Write its goldens:
   `go test ./internal/platformlife -run TestRenderGoldenOfPreviousBOMs -update`.
   Each new file under `testdata/previous/X.Y.Z/` must equal the file of the
   same name under `internal/platformlife/testdata/` at the tag (`git show
   vX.Y.Z:internal/platformlife/testdata/<name> | cmp - <new file>`). A
   difference means a renderer changed since the tag and this build would
   change that platform at its next apply.

What holds the procedure:

- `TestEveryReleaseHasItsPreviousBOM` fails when `CHANGELOG.md` lists a release
  or a release candidate from 0.8.0 on, other than the one the current BOM names, without its file. It
  reads the changelog, not the tags: a release without a changelog section is
  not seen.
- `TestPreviousBOMsAreUnchanged` compares each file with its recorded hash and
  fails on a release it has no hash for, a `-dev` one included. The script test
  `test_the_previous_boms_parse_and_are_releases` checks that no previous BOM is
  a `-dev` one and that each file is named after its release.
- `bom-verify.yml` (pull requests that touch `deploy/platform/**`, weekly) and
  the `bom-verify` job of `docker.yml` on a `v*` tag run `bom_verify.py
  --release --bom` on each previous BOM: an apply on an installed platform pulls
  by these pins, so a release whose images carry a previous BOM with a pin that
  is no longer served is not built. `helm.yml` checks the current BOM only. A
  chart version that a previous BOM pins must stay in the
  [chart mirror](#chart-mirror) and a VM image it pins must stay published,
  also after `charts-mirror.yaml` and the current BOM moved on.

Only releases are kept. A `dev-<sha>` build of `main` installs with a `-dev` BOM
that no later build carries, so its platform is refused after an upgrade
([managed-services.md](managed-services.md#not-implemented-yet)). No file has
been removed yet; when one is, the platforms of that release are refused again,
so it needs a release note.

The previous BOMs are `0.8.0`, `0.8.1`, `0.8.2`, `0.9.0-rc.1`, `0.9.0-rc.2`, `0.9.0`, `0.9.1`, `0.10.0-rc.1`, `0.10.0`, `0.10.1-rc.1`, `0.10.1-rc.2` and `0.10.1`. `0.8.0` was added by hand from its tag
(sha256 `8eb874987d3bbaf55f309295ee241ec320c0cf9e0805b3bd1812ef908b6600c6`, the
value a platform installed by v0.8.0 records). `0.8.1` is the first one the
procedure above added (sha256
`a635bd88d3c39baf5519344088a6fed8dad8cb2b3c2d324655753e8ec55719b3`): it was
copied from the release commit of `v0.8.1` (`aad9fb46`), which the tag is put
on, before the tag existed, and its goldens equal those of that commit and
differ from the 0.8.0 ones in the release label only. `bom_verify.py --release`
printed `all pins match` on both on 2026-11-18.

The tag `v0.8.1` is on that commit: `git show v0.8.1:deploy/platform/platform-bom.yaml | sha256sum`
gives the hash above (checked on 2026-11-18).

`0.8.2` was added the same way (sha256
`a10baf0bef4f33ebff26cb87315259a6cf886c8ffe00b79dc24e2bed4ed6540b`): it was
copied from the release commit of `v0.8.2` (`ed4b94ec`), which the tag is put
on, before the tag existed, and its goldens equal those of that commit and
differ from the 0.8.1 ones in the release label only. `bom_verify.py --release`
printed `all pins match` on the three previous BOMs on 2026-11-18. The tag
`v0.8.2` is on that commit: `git show v0.8.2:deploy/platform/platform-bom.yaml | sha256sum`
gives the hash above (checked on 2026-11-19).

`0.9.0-rc.1` is the first previous BOM of a release candidate, and the first
whose pins differ from those of `0.8.0`: cluster chart 0.1.14 and platform
chart 0.1.3 (sha256
`731ffb6217aefd5dcca55085d8de47f60968afa37ec951830b6d3eb50bc75af8`). It was
copied from the tag `v0.9.0-rc.1` (`eee7b3dc`), after the tag existed:
`git show v0.9.0-rc.1:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`),
and differ from the 0.8.2 ones in the release label and in the versions of the
two charts. `bom_verify.py --release` printed `all pins match` on the four
previous BOMs on 2026-11-19.

`0.9.0-rc.2` has the pins of `0.9.0-rc.1` (sha256
`bd512541501290813d841c7ac0338ca59a41c86768b3856b51f90db20dcc7808`). It was
copied from the tag `v0.9.0-rc.2` (`8906fa93`), after the tag existed:
`git show v0.9.0-rc.2:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`),
and differ from the 0.9.0-rc.1 ones in the release label only.
`bom_verify.py --release` printed `all pins match` on the five previous BOMs on
2026-11-19.

`0.9.0` has the pins of the two candidates (sha256
`ca7140045f0bf3951b66a371b22a683ab54ec063ccf9b73562285f4238aa442d`). It was
copied from the tag `v0.9.0` (`ed82439a`), after the tag existed:
`git show v0.9.0:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`),
and differ from the 0.9.0-rc.2 ones in the release label only.
`bom_verify.py --release` printed `all pins match` on the six previous BOMs on
2026-11-19.

`0.9.1` has the pins of `0.9.0` (sha256
`a9bbf835402a2fd4e13992ff389856c5be527498588f8e0a6bb0760a664ca2cd`). It was
copied from the tag `v0.9.1` (`5ceca5e0`), after the tag existed:
`git show v0.9.1:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`),
and differ from the 0.9.0 ones in the release label only.
`bom_verify.py --release` printed `all pins match` on the seven previous BOMs on
2026-11-19.

`0.10.0-rc.1` is the first previous BOM whose pins are those of `main` and not of
an older release (sha256
`d43dc17287bebc9ea097acbcc4f3db9a37f1957eeed488ce071b6de5c1dd0412`): cluster
chart 0.1.15 and database chart 0.5.2, where `0.9.1` has 0.1.14 and 0.5.1. It was
copied from the tag `v0.10.0-rc.1` (`12492cb2`), after the tag existed:
`git show v0.10.0-rc.1:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`).
`bom_verify.py --release` printed `all pins match` on the eight previous BOMs on
2026-11-20. The files under `previous/` are read in the order of their names, and
`0.10.0-rc.1` sorts before `0.8.0`: a test that needs the oldest BOM asks for
`0.8.0` by its release or its hash, not for the first file.

`0.10.0` has the pins of `0.10.0-rc.1` (sha256
`721b9992f2663c033cae4eed431511686c6178e3ea520c61ea3945aa07199817`). It was
copied from the tag `v0.10.0` (`8797f8ac`), after the tag existed:
`git show v0.10.0:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`),
and differ from the 0.10.0-rc.1 ones in the release label only.
`bom_verify.py --release` printed `all pins match` on the nine previous BOMs on
2026-11-20. It sorts before `0.8.0` too.

`0.10.1-rc.1` is the first previous BOM with other Kubernetes pins than the ones
before it: k3s `v1.36.6+k3s1`, Kubernetes 1.36.6 and its node image (sha256
`3065954af13509552321c91c693a07e45bfa767344f39a9c0b890d27297b0621`). It was
copied from the tag `v0.10.1-rc.1` (`dd14e17c`), after the tag existed:
`git show v0.10.1-rc.1:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`).
`bom_verify.py --release` printed `all pins match` on the ten previous BOMs on
2026-11-20. It sorts before `0.8.0` too. It keeps cert-manager v1.20.2 and
Cilium 1.18.16 on Kubernetes 1.36, outside the versions their own documents
list: accepted as "to be proven by the lab drill", and not proven yet.

`0.10.1-rc.2` has the Kubernetes pins of `0.10.1-rc.1` and cluster chart 0.1.16
where that one has 0.1.15, for `pc-platform` and for `tenantCharts.kubernetes`
(sha256 `14acba7b0d7ae05bda488066eb2f1699f73139bc78ae44df824a758e587d17bd`). It was
copied from the tag `v0.10.1-rc.2` (`ef44cdaf`), after the tag existed:
`git show v0.10.1-rc.2:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`).
`bom_verify.py --release` printed `all pins match` on the eleven previous BOMs
on 2026-11-21. It sorts before `0.8.0` too. An installation whose machines are
being replaced toward `0.10.1-rc.1` records it as its target after a Retry on
the candidate's build, so a later build needs this file to go on with it.

`0.10.1` has the pins of `0.10.1-rc.2` (sha256
`aec05ea60e6bfd12d73bab3d45a6a2aae8a3f62f3a41d3f43737aa8ef1d74e3b`). It was
copied from the tag `v0.10.1` (`7cb4bee0`), after the tag existed:
`git show v0.10.1:deploy/platform/platform-bom.yaml | sha256sum` gives that
hash. Its goldens equal the current goldens at the tag, file for file (`cmp`),
and differ from the 0.10.1-rc.2 ones in the release label only.
`bom_verify.py --release` printed `all pins match` on the twelve previous BOMs
on 2026-11-21. It sorts before `0.8.0` too. An installation whose machines are
being replaced toward `0.10.1-rc.1` or `0.10.1-rc.2` records `0.10.1` as its
target after a Retry on the release's build, so a later build needs this file to
go on with it.

Not verified: an apply on a platform that `v0.8.1` or `v0.8.2`
installed, an install by `v0.8.1` or `v0.8.2`, and an apply on a platform that
is on the BOM `0.9.0-rc.1`, `0.9.0-rc.2`, `0.9.0`, `0.9.1`, `0.10.0-rc.1`, `0.10.0`, `0.10.1-rc.2` or `0.10.1`, installed by that build or moved to it, and a whole move to `0.10.1-rc.1` (its drill on one lab, 2026-11-20, did part "seed" and stopped in part "platform").
One apply with a
previous BOM was run on a lab (2026-11-18, a platform installed by the released
`v0.8.0`, on the build `dev-751a190` of `main`): it succeeded and `status.bom`
stayed the 0.8.0 one. That build's own BOM pinned the same versions as 0.8.0, so
an apply that keeps older pins against a BOM with other pins is not shown.
The same platform was read on the build `dev-03bc9f3` of `main` the same day,
which carries two previous BOMs (`0.8.0`, `0.8.1`): the plan had no change and
named the build's own BOM as `newer`, `status.bom` stayed the 0.8.0 one, and
`GET /admin/platform/bom?sha256=` answered both previous BOMs by their hash. No
apply was run on that build.


## What a release checks about its BOM pairs

An installed platform can be moved from the BOM of an earlier release to this
one's ([managed-services.md](managed-services.md#upgrade-between-boms)). A BOM
pair is a previous BOM under `deploy/platform/previous/` and the BOM of the
commit. `platformlife.Classify` decides per pair whether the move is allowed:
the release line, the tenant charts and the versions of the product's own
charts may differ, and so may the seed's k3s and the Kubernetes version with
its node image, each by a patch or by one minor up; and cert-manager's version,
in a pair that changes no k3s, Kubernetes version or node image. `go test
./internal/platformlife` holds the rest:

- **`upgradeFrom` is a statement the release makes.** A component of the
  product's own charts whose version changes is moved only when its BOM entry
  lists the old version in `upgradeFrom`. Add the old version when, and only
  when, the checks below pass for it. Without it the pair is refused and the
  platforms of that release stay on their BOM, which is safe.
- **No machine rollout (`TestUpgradeRollsNoMachine`).** For every pair the
  classifier allows, the test renders the cluster chart of the previous BOM with
  the values that release rendered
  (`testdata/previous/<release>/render-cluster-values.golden.yaml`) and the
  cluster chart of this BOM with this build's values
  (`testdata/render-cluster-values.golden.yaml`), and compares the whole
  KubeadmControlPlane and every machine and bootstrap template as rendered,
  metadata included, and every MachineDeployment's machine template. It is as
  strict as the run's own check before the sync, which fails on any of these
  objects ArgoCD does not report Synced: a new label or annotation on the
  KubeadmControlPlane counts, though it replaces no machine. A difference means
  Cluster API would replace machines, or every upgrade run would fail
  `unexpected_rollout`: do not list
  the old version in `upgradeFrom` of `pc-platform`. The values come from Go
  (`ClusterValues`), so a renderer change is caught as well as a chart change. A
  chart version that is not the one in the tree is pulled from the registry
  (`helm pull`): the test needs `helm` and, for such a pair, the network; in CI
  it fails without them, elsewhere it is skipped.
  A pair that replaces machines by design (class `machines`: another Kubernetes
  version or node image) is compared where the comparison must hold: this
  build's chart is rendered with the values a run renders before the roll
  begins (step 0: the previous BOM's Kubernetes version, kube-vip and
  templates) and must equal what the previous release rendered. The run applies
  the new chart version with step 1 of the roll, whose check lets only that
  step's unit differ, so a chart or renderer that changed any other machine
  object would fail every such upgrade at step 1 (`unexpected_rollout`). The
  test also fails when step 0 already renders the target's machines. The steps
  after 0 are `TestRollRendersOneUnitAtATime`'s. The comparison of a pair that
  replaces no machine is unchanged.
- **A cert-manager move was read (`TestCertManagerMoveRenders`).** Every
  cert-manager row a pair allows must be a move listed in `certManagerReads`
  (`upgrade_chart_test.go`), where the release records what it read in
  upstream's upgrade notes for that pair. The test pulls both chart versions
  (from the chart mirror by the BOM's digest; a version no BOM pins yet from
  upstream by its index digest), renders each with the values a run gives the
  seed's and the platform cluster's Application at the BOM's Kubernetes
  version, requires every CRD at the one version `v1`, served and stored, and
  the objects the new chart stops and starts rendering to be exactly the ones
  listed. List the old version in cert-manager's `upgradeFrom` only with such
  an entry.
- **The plan as the operator reads it (`TestUpgradePlanGolden`).** Goldens under
  `testdata/upgrade/0.8.1-to-<case>/plan.golden.json`: 0.8.1 to a next release
  with the own charts bumped (allowed), with a third-party chart or a node image
  bumped next to them (refused), with cert-manager alone (allowed) and with
  cert-manager next to a k3s or a node image move (refused). They are fixtures, built from the 0.8.1 BOM, and
  do not move with the BOM on `main`. `TestClassifyRefuses` has one case per kind
  of refused entry.
- **A `-dev` BOM is never a target.** The BOM on `main` is refused as a target
  until the release commit drops `-dev`.

What a release must do when it changes a pin, before its tag:

1. A change of `proxcloud-platform` or `proxmox-kamaji-cluster`: bump the chart,
   tag it, set the pin and its digest, and decide `upgradeFrom` by the checks
   above. A chart that changes a machine or bootstrap template must not list the
   old version: the release notes then say that installed platforms stay on
   their BOM.
2. The seed's k3s, the Kubernetes version or the node image: the move is allowed
   by a patch or one minor up and replaces the seed's k3s binary or every
   machine of the platform cluster. The release notes say what stops serving at
   each step, which pinned pieces do not list the new Kubernetes version in
   their own documents, and which starting BOMs were drilled. The installer's
   `pc_versions.k3s` and `pc_defaults.services.k8s_version` need not follow:
   `TestBOMMatchesInstallerVersions` lets them stay up to one minor behind the
   BOM and never ahead of it (the kubectl in the clusterctl image follows the
   second one and is supported one minor from the API server).
3. cert-manager: read upstream's upgrade notes for the pair (one minor at a
   time), add the move to `certManagerReads`, and list the old version in
   `upgradeFrom` of both entries. A release that also moves k3s, Kubernetes or
   the node image refuses it: move cert-manager in a release of its own.
4. Any other pin (an image, a provider, another third-party chart): every platform of
   an earlier release is refused the move. Say so in the release notes; the
   platforms keep working on their BOM.
5. Read the plan the release gives a platform of each earlier release: on a
   build of the release commit, `GET …/plan` of such an installation, or
   `PlanFor` in a test.

The pairs of the BOM on `main` today (the eleven below and 0.10.1 to it). `main` has the pins of the release
`v0.10.1`, so the pair from 0.10.1 differs in the release line alone: `Classify` gives one change, the release,
record only, with no seed part and no machine replaced. The eleven older pairs are the ones the release had, below.
`TestUpgradeRollsNoMachine` compares all twelve and passes, nine of them at step 0 of a roll, and
`TestPreviousBOMsMoveAsTheirPinsDiffer` holds each pair to its split (2026-11-21). A development BOM carries nothing:
on a build of `main`, an installation that stands in the replacement of its machines toward 0.10.1-rc.1,
0.10.1-rc.2 or 0.10.1 goes on to that target with a Retry, by the kept file, with nothing to confirm
(`TestRollTowardAKeptCandidateGoesOn`). Only `-dev` refuses the twelve pairs on `main`.

The pairs of the BOM of the release `v0.10.1` (the ten below and 0.10.1-rc.2 to it). It has the pins of the
candidate `v0.10.1-rc.2`, so the pair from 0.10.1-rc.2 differs in the release line alone: `PlanFor` gives one change,
the release, record only, with no seed part and no machine replaced. The ten older pairs are the ones the candidate
had, below, with the release `0.10.1` in place of `0.10.1-rc.2`: 3 changes from 0.10.1-rc.1, 19 from 0.10.0-rc.1 and
0.10.0, 20 from 0.9.0, 0.9.1 and the candidates of 0.9.0, 23 from the three 0.8.x BOMs, each allowed, the nine
older ones in two parts, with no refusal. `TestUpgradeRollsNoMachine` compares all eleven and passes, nine of them at
step 0 of a roll, and `TestPreviousBOMsMoveAsTheirPinsDiffer` holds each pair to its split (2026-11-21, on the
release commit). A release BOM carries an unfinished target: an installation on 0.10.0 that stands in the
replacement of its machines toward 0.10.1-rc.1 or 0.10.1-rc.2 after a step goes on to 0.10.1 with a Retry,
`carried` the candidate, 19 changes, nothing to confirm (`TestRollTowardAKeptCandidateGoesOn`). One whose seed part
ran under a candidate and whose replacement has not begun goes on to that candidate with an apply without a target,
and the plan offers 0.10.1 as `replacement`, with the phrase `upgrade <name> to 0.10.1 and replace its machines`
(`PlanFor`, 2026-11-21). No move to 0.10.1 was run anywhere.

The pairs of the BOM of the candidate `v0.10.1-rc.2` (0.8.0, 0.8.1, 0.8.2, 0.9.0-rc.1, 0.9.0-rc.2, 0.9.0, 0.9.1, 0.10.0-rc.1, 0.10.0 and
0.10.1-rc.1 to it). It has the Kubernetes pins of the candidate `v0.10.1-rc.1` and cluster chart 0.1.16 where that one
has 0.1.15, so the pair from 0.10.1-rc.1 differs in the release line, in `tenantCharts.kubernetes` and in `pc-platform`,
0.1.15 to 0.1.16: `PlanFor` gives three changes, two record only and one of class chart (stage `cluster`, wave 3), with
no seed part and no machine replaced. The nine older pairs each carry the move to Kubernetes 1.36.6 below, with
`pc-platform` to 0.1.16: 19 changes from 0.10.0-rc.1 and 0.10.0 (the 17 of the first candidate, the tenant chart and
`pc-platform` 0.1.15 to 0.1.16), 20 from 0.9.0, 0.9.1 and the candidates of 0.9.0, 23 from the three 0.8.x BOMs, each
allowed, in two parts, with no refusal. `pc-platform` lists 0.1.13, 0.1.14 and 0.1.15 in `upgradeFrom`.
`TestUpgradeRollsNoMachine` compares all ten and passes, nine of them at step 0 of a roll (cluster charts 0.1.13,
0.1.14 and 0.1.15 pulled from the registry), and
`TestPreviousBOMsMoveAsTheirPinsDiffer` holds each pair to that split: no seed or machines row without another k3s or
Kubernetes pin, and no such pin without its row (2026-11-20). An installation that stands in the replacement of its
machines toward 0.10.1-rc.1 (installed 0.10.0, `roll.step` 1 to 4) gets, from `PlanFor` without a target, the move to
0.10.1-rc.2 as the upgrade in progress: `carried` 0.10.1-rc.1, 19 changes, nothing to confirm, part `seed` done, part
`platform` allowed; a Retry records 0.10.1-rc.2 as the target and keeps the step. Before the replacement has begun
(seed part done, no step) and after its last unit an apply without a target goes on to 0.10.1-rc.1, and the plan
offers the move to 0.10.1-rc.2 as `replacement` (before the replacement has begun with the phrase
`upgrade <name> to 0.10.1-rc.2 and replace its machines`). None of these moves was run anywhere.

The pairs of the BOM of the candidate `v0.10.1-rc.1` (the nine before it to
it). All nine differ from it in the seed's k3s (`v1.35.10+k3s1` to `v1.36.6+k3s1`), in the Kubernetes version (1.35.10 to
1.36.6) and in its node image, each one minor up: every pair replaces the seed's k3s and every machine of the platform
cluster, and `TestUpgradeRollsNoMachine` compares all nine at step 0 of the roll and passes (2026-11-20; cluster chart
0.1.13 and 0.1.14 pulled from the registry). On the candidate's commit, `PlanFor` on an installation of
one control plane machine and one machine per group gives, from each of the nine: allowed, no refusal, two parts (`seed`
with the phrase `upgrade <name> to <release>`, `platform` blocked by `seed_not_upgraded` until the seed's k3s has moved
and then with `upgrade <name> to <release> and replace its machines`), the row `roll`, `seed.k3s` with four `site` rows, two `machines`
rows, four `roll/<unit>` and four `roll-vm/<unit>` rows; 17 changes from 0.10.0-rc.1 and 0.10.0, 20 from 0.9.0, 0.9.1 and
the candidates of 0.9.0, 23 from the three 0.8.x BOMs (read again on 2026-11-20). None of these moves was run anywhere. The differences next to
those three entries are the ones the BOM of 0.10.0 had. From the three 0.8.x BOMs, the BOMs
differ in the release line, in `tenantCharts.kubernetes` and `tenantCharts.database` and in the four
components of the product's own charts, `pc-platform` 0.1.13 to 0.1.15 and
`capi`, `capi-tenant` and `platform` 0.1.2 to 0.1.3. Each of the four lists the
old version in `upgradeFrom`, and platform chart 0.1.3 differs from 0.1.2 in the provider's
ClusterRole alone. From 0.9.0-rc.1, 0.9.0-rc.2, 0.9.0 and 0.9.1 the BOMs differ in the release line, in the
two tenant charts and in `pc-platform`, 0.1.14 to 0.1.15. `pc-platform` lists both 0.1.13 and 0.1.14 in
`upgradeFrom`: a build takes only its own BOM as a target, so dropping 0.1.13 would leave a 0.8.x platform
no move without a 0.9 release in between. From 0.10.0-rc.1 and 0.10.0 the BOMs differ in the release line alone.
So the classifier allows all nine pairs. On the release commit of `v0.10.0` the plan from each
of the three 0.8.x BOMs is allowed with seven changes and no refusal, the plan from 0.9.0, 0.9.1
and each candidate of 0.9.0 with four changes, and the plan from 0.10.0-rc.1 with one change, the
release, record only (`Classify` on the eight pairs, 2026-11-20). No move to 0.10.0 was run
anywhere. On the commit of the candidate `v0.10.0-rc.1` the plan from each
of the three 0.8.x BOMs is allowed with seven changes and no refusal, and the plan from 0.9.0,
0.9.1 and each candidate of 0.9.0 with four changes: the release and the two tenant charts, record
only, and `pc-platform` 0.1.14 to 0.1.15, class chart, stage `cluster` (`PlanFor` on the seven
pairs, 2026-11-20). One of these moves was run on a lab: 0.9.1 to 0.10.0-rc.1, on 2026-11-20
(22.8 seconds, no machine replaced). On the release
commit of `v0.9.1` the plan from each of the three 0.8.x BOMs is allowed with six changes
and no refusal, and the plan from 0.9.0 and from each candidate's BOM with one change, the
release, record only (`Classify` on the six pairs, 2026-11-19). On the release
commit of `v0.9.0` the plan from each of the three 0.8.x BOMs is allowed with six changes
and no refusal, and the plan from each candidate's BOM with one change, the release,
record only. On the
commits of the candidates `v0.9.0-rc.1` and `v0.9.0-rc.2` the plan of each of the three
0.8.x BOMs is allowed, with six changes and no refusal; on that of `v0.9.0-rc.2` the plan
from 0.9.0-rc.1 is allowed with one change, the release, record only. One of these moves
was run on a lab: 0.8.0 to 0.9.0-rc.1, on 2026-11-19, on a platform without tenants (72.8
seconds, no machine replaced). The others were not; 0.8.0 to 0.9.0 was run on one
production site, and no move to 0.9.1 anywhere.

Not checked: that a chart of `proxcloud-platform` whose version changes is safe
to sync over the old one (its templates are RBAC, AppProjects, a MetalLB pool
and the clusterctl hook; review the diff). A chart version that changes its hook alone is recorded
as moved without the hook running: ArgoCD syncs only an Application whose rendered objects differ,
and a hook is not among them (seen on the lab on 2026-11-19). Such a version must also change a
rendered object, or the run must learn to ask for the sync.

### Upgrade drill builds

A lab drill of an upgrade needs two builds whose BOMs form an allowed pair, and
the second must carry the first's BOM. Development BOMs do not work for either
side: a platform installed by a `-dev` build is on a BOM no other build carries
(`bom_unsupported`), and a `-dev` BOM is refused as a target. So a drill build
is a release candidate cut from `main` ([Release candidates](#release-candidates)), or, for a
BOM change that `main` does not have, a build made like a release on a throwaway branch that is never merged:

1. Install the lab platform from a release (v0.8.0, v0.8.1 or v0.8.2), or from the
   drill build before this one.
2. On the branch, set `release:` in `deploy/platform/platform-bom.yaml` to a
   version without `-dev` that no release will use, for example
   `0.9.0-drill.1`, and make the BOM change to drill (none for a record-only
   move). Every digest must be real: a release BOM takes no `pending`.
3. For the build after it (`0.9.0-drill.2`), copy the BOM file of the first
   drill build byte for byte to `deploy/platform/previous/0.9.0-drill.1.yaml`,
   add its sha256 to `previousSHA256` in `internal/platformlife/bom_test.go` and
   write its goldens, as for a release ([Previous BOMs](#previous-boms)). A
   build that lacks the file refuses the platform with `bom_unsupported`.
4. Build the images from the branch and run them on the lab only.

Not determined: whether the image workflows publish a build for such a branch
(`docker.yml` publishes `dev-<sha>` for pushes to `main`), and whether
`bom-verify` accepts a `-drill` release name. Build the images by hand on the
lab if they do not.

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

## Release notes

The body of a GitHub Release is `docs/releases/vX.Y.Z.md` when the tag has that
file (`docker.yml`, job `release`, step "Assemble release notes"). Without the
file the step prints a warning and falls back to the `## [X.Y.Z]` section of
`CHANGELOG.md`, as every release up to `v0.8.2` did; the script test below fails
when a changelog section of a release from `0.8.0` on has no file, so only an rc tag
without a file takes that path (`v0.9.0-rc.1`, `v0.9.0-rc.2`, `v0.10.0-rc.1`, `v0.10.1-rc.1` and `v0.10.1-rc.2` have one). An empty file, or one over 120000 characters, fails
the step, and no Release is created. GitHub's generated commit and pull request
list (`--generate-notes`) follows the body either way.

`CHANGELOG.md` stays the complete record: every entry, with its detail, its
file names and what was and was not verified. The release notes are the page a
person reads in five minutes, and must say nothing the changelog section does
not. Write the file in the release pull request, with the changelog section:

1. Start with one or two sentences that say what the release is. No `#` title: the Release page has one.
2. Then `##` sections, in this order, each only when it has content: Highlights, Upgrade, What changed, Verified on a lab, Not verified, Known issues. Highlights and Upgrade are required.
3. Highlights: three to seven bullets, a bold lead of two to five words, then the effect a user sees. No implementation detail.
4. Upgrade: numbered steps with the commands in fenced code blocks, and a table "If you run ... / Do ..." for steps that depend on the site.
5. What changed: `###` groups by area (Operator console, Customer console, Managed services, Kubernetes, Networking, Security, Installer), bullets of one or two short sentences, with a link to the guide for the detail.
6. Known issues: for each, what happens, who is affected and what to do.
7. Never drop an upgrade step, a known issue or a "not verified" statement to make the page shorter.
8. No hard line wraps: one paragraph or one list item is one source line. A Release body renders every newline as a line break, which is what made the changelog sections hard to read there.
9. Plain words, sentences of about 30 words at most, code and field names in backticks, no finding ids and no `file:line`.
10. Links are absolute and name the release's tag, since a Release body has no relative base: `https://github.com/mutex-solutions/proxcloud/blob/vX.Y.Z/docs/...`. End with a line that links the changelog section and the guides.

`.github/scripts/tests/test_release_notes.py` (in `test.yml`, job `installer`)
fails when a `## [X.Y.Z]` section of `CHANGELOG.md` from `0.8.0` on has no file,
and checks every `docs/releases/v*.md`: its version has a section in `CHANGELOG.md`,
it has Highlights and Upgrade, no line continues the paragraph or list item
before it, every link is absolute, a link into this repository is a `blob/` or a
`releases/tag/` one, and every `blob/<tag>/<path>` link names the file's own tag
and a path that exists in the tree. It does not check that an anchor exists, that the path
exists at the tag (the tag is cut later), or that the text agrees with the
changelog: review does.

To see what the Release page will show, render the file as GitHub renders a
Release body and read the HTML (no `<br>` inside a paragraph or a list item):

```sh
gh api -X POST /markdown -f mode=gfm -f context=mutex-solutions/proxcloud -F text=@docs/releases/vX.Y.Z.md
```

The Releases `v0.8.0`, `v0.8.1` and `v0.8.2` were created from their changelog
sections. Their files are in `docs/releases/`, written afterwards; a published
Release is changed by hand (`gh release edit vX.Y.Z --notes-file docs/releases/vX.Y.Z.md`
replaces the whole body, the generated list included), not by a workflow.

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
