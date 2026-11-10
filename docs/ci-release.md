# CI, template builds and release operations

This page describes the checked-in workflows, not a guarantee that a release has
already been published. As of **2026-11-10**, GHCR has public development images;
ProxCloud has no versioned GitHub Release/tag or published OCI chart. Read
[release readiness](release-readiness.md) before planning the first publication.

## Workflow map

| Workflow | Triggers | Effect / runner |
|---|---|---|
| `test.yml` | PRs targeting `main`; pushes to `main` | Hosted Go unit/integration tests, both SPA build/smoke tests, installer syntax/tests, credential-free Packer validation and offline image-pipeline tests, naming gates. Not triggered by release tags. |
| `docker.yml` | PRs to `main`, `main` pushes, `v*` tags, manual dispatch | PRs build five images without pushing. Image-relevant `main` pushes publish `dev-<7-char-sha>`; docs/chart-only changes can skip. Tags publish exact image tags; manual dispatch pushes `dev-<sha>` or `dev-<suffix>`. Hosted runner. |
| `helm.yml` | Chart-path PRs to `main`; app or managed-chart tags | PR lint/render only. App `v*` publishes `proxcloud`; `database-cluster-v*` and `proxmox-kamaji-cluster-v*` publish their independent charts. Hosted runner; no publication on ordinary merge. |
| `sandbox-templates.yml` | Selected source pushes/PRs, schedule, manual dispatch | PR read-only build; trusted runs push built-in images, then `main` opens a digest-pin PR. Not cluster deployment. Hosted runner. |
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
`oci://ghcr.io/mutex-solutions/charts`; no app chart has been published there yet.
On an app release tag, the workflow sets chart version to the tag without `v` and
appVersion to the full tag, matching the image tags. Managed charts use their own
`Chart.yaml` versions and reject a mismatching tag. Re-publishing an already
existing chart version is skipped; it does not replace old contents. See the
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
for credentials with cloud mutation rights.

Both PVE workflows share concurrency group `vm-templates`, intentionally
serializing their shared network/build resource use. Two registered runners do
not imply two concurrent template builds are safe. Before enabling final routing
labels, review/cancel stale queued runs: assigning a matching runner can start an
old workflow immediately. Runner registration must not accidentally execute an
all-OS scheduled run when only one canary was approved.

## Current release-gate limitations

The workflows currently **do not** provide a single atomic release gate:

- Docker publication and `test.yml` are independent. Tag pushes do not run the
  test workflow. Choose an exact commit whose tests completed successfully.
- GitHub Release creation waits for all five image builds, but not for the
  separate Helm publication workflow. Verify both before telling users to install.
- The Release job calls `gh release create ... --latest` without explicit
  prerelease handling. A tag suffix such as `-rc.1` alone is not a reviewed
  prerelease/non-latest policy. Fix that behavior before the first RC.
- No workflow automatically installs/upgrades a production site. Publication is
  not deployment, acceptance, backup verification or rollback readiness.

This page documents those limitations; it does not silently change workflow
behavior. Routine docs updates must not create tags or trigger a real PVE build.
For the first preview, use the checklist in
[release readiness](release-readiness.md#minimum-gate-before-a-first-previewrc).
