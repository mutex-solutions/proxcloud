# VM artifact export and publishing

## Status and scope

One manual run of [`vm-artifact-export.yml`](<../.github/workflows/vm-artifact-export.yml>) takes a template on the lab to a published, attested and publicly verified VM image. Every step that produces or publishes bytes is a workflow in this repository; nothing is prepared by hand. The earlier flow, where an operator filled a directory on the runner and typed a run id and two hashes into a second workflow, is removed (`vm-artifact-stage.yml` and `images/vm_stage_local.py` no longer exist, and the publisher has no manual trigger).

**What has run, and what has not (2026-11-14).** The driver was run on a workstation, stage by stage as the workflow runs it, with the CI-built tool image pulled by digest and the private copy of the Kubernetes 1.37.2 template: `plan`, `config`, `recover`, `tools`, `inventory`, `review`, `sanitize`, `postscan`, `bundle`, `evidence`, then `recover` and `purge`. The derived review is byte-identical to the one used for the published 1.37.2 image, and the bundle and the evidence record pass the publisher's and the verifier's own checks. In that run the copy stage was replaced by the earlier guarded copy and its receipt, and the cold boot by a synthetic result: no PVE API was called. The workflow itself, the template guard through the API from the export runner, the cold boot from that runner, the publisher inside the same run, the attestations and the anonymous verification have **not** run yet, and no native template has gone through the driver. The first real run is their acceptance; [what it has to confirm](#what-only-a-real-run-can-confirm) is listed below. The settings that keep other workflows off the export runner are the owner's and are [not checked by any run](#trust-model).

The workflow exports native OS templates (`variant: os`) of `ubuntu-2404` and `ubuntu-2604`, and Kubernetes node templates (`variant: kubernetes`, `os: ubuntu-2604`, `kubernetes_version` one of `1.35.10`, `1.36.6`, `1.37.2`: the allowlist `K8S_VERSIONS`, the same in the publisher, the sanitizer and the copy policy, kept equal by `images/tests/test_k8s_versions.py`). Debian is accepted by the artifact contract but is not an input of this workflow. Kubernetes images publish to `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s`, native ones to `ghcr.io/mutex-solutions/proxcloud-vm-<os>`.

**Earlier publications.** Images published before this workflow (for example `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s:ubuntu-2604-k8s-1.35.10-20261112032843-8a2a737b331b`) were prepared with manual steps. They stay where they are. An export through this workflow gets a new tag (the build time in the tag is the export run's time), and that CI-built digest is the one to use once it exists. Publication is not node acceptance: no kubeadm join or NodeReady is claimed.

## The flow

The inputs are `variant`, `os`, `kubernetes_version` (`none` for a native export) and `template_vmid`. No path, URL, tag, node, storage, command, hash or run id is an input.

| Job | Runner | What it does | Credentials it holds |
|---|---|---|---|
| `export` | self-hosted, labels `self-hosted, linux, x64, proxcloud-vm-artifact-export`, environment `vm-artifact-export` | The stages of [`images/vm_export/pipeline.py`](<../images/vm_export/pipeline.py>), then two artifact uploads | The PVE token, only in the steps that call the API. `contents: read`. No registry credential, no `packages` permission |
| `publish` | GitHub-hosted | The reusable publisher [`vm-artifacts.yml`](<../.github/workflows/vm-artifacts.yml>): authorizes the export job, downloads and re-verifies the bundle, pushes it with ORAS, records the digest of that push | The job token with `contents: read`, `actions: read`, `packages: write` |
| `attest` | GitHub-hosted | A build-provenance attestation for the pushed digest, then `gh attestation verify` on it with the command the summary prints; a second attestation for the sha256 of the evidence record | `id-token: write`, `attestations: write`, `packages: write` |
| `verify` | GitHub-hosted | Pulls the digest with no credential, verifies the three files, checks the evidence record against its attested sha256, writes the final summary | `contents: read` only: no token in the environment, no login |

The `export` job runs one driver stage per step. A stage refuses unless exactly the earlier ones completed, and each one checks the record the previous one wrote:

1. **plan**: validates the four inputs and the run identity (this repository, `main`, a manual dispatch, **run attempt 1**). It reads no secret and calls no API. It fixes the build version (the run's UTC time) and the artifact name. A re-run of the export job is refused (`rerun_refused_dispatch_a_new_run`): it would export at the commit of the first attempt, however far `main` has moved since. Dispatch a new run instead. Re-running only the hosted jobs of a run is fine; they reuse the export job of attempt 1.
2. **config**: validates the runner configuration (the secrets and the variables below) without any API call, and masks the node name, the gateway, each name server and each test address in the log. It also refuses a secret that could hide a job output; see [masked outputs](#masked-values-and-job-outputs).
3. *(not a stage)* **recover**: the guarded cold-boot cleanup of anything an earlier run left on the lab; see [cold-boot state](#cold-boot-state).
4. **tools**: `docker pull` of the [pinned tool image](#export-tool-image) by registry digest, with an empty Docker client configuration.
5. **copy**: the PVE template guard (a stopped, unlocked template in pool `pc-templates` with the exact name and tags its builder gives it, no running task, its own base volume), a read-only copy of that volume from the storage mount (which must be mounted read-only), then the guard again: both snapshots must be equal. The EFI disk and the cloud-init drive of a Kubernetes template are never read. A Kubernetes root disk is a raw file of exactly 20 GiB; a native one is a QCOW2 **file**, which is larger than its 20 GiB virtual size (21478375424 bytes on the lab) and is bounded as the sanitizer bounds it (24 GiB). This stage also decides the [upstream checksum](#upstream-checksum) the manifest will name.
6. **inventory**: a read-only capsule over the private copy.
7. **review**: Kubernetes: the review is derived from the committed baseline; only the Kubernetes package versions may differ from it. Native: the eligibility gate (a clean, accepted inventory of exactly the copied file that says the image can be sanitized).
8. **sanitize**: the offline sanitizer in a capsule; the disk is sealed only after the container is proven stopped.
9. **postscan**: an independent read-only capsule over the sealed disk.
10. **coldboot**: two clones through the PVE API, fresh cloud-init, guest checks over SSH, different identities, guarded cleanup ([details](<public-vm-sanitization.md>)).
11. **bundle**: `vm_artifact.prepare` writes `manifest.json` and `SHA256SUMS`. `build_version` is the export run's time, `source_commit` is the workflow's commit and `run_ref` is this run's URL. The stage then checks the directory exactly as the publisher will.
12. **evidence**: builds the one [public evidence record](#public-evidence) and reports its sha256.

Then the job uploads the bundle (three files, compression level 0, named `vm-export-<os>-<UTC build>-<12 hex>` or `vm-k8s-export-ubuntu-2604-<UTC build>-<12 hex>`) and, separately, the evidence record (`<bundle name>-evidence`). Two steps always run at the end, also after a failure or a cancel: `recover` repeats the guarded cold-boot cleanup, and `purge` deletes the private copy, the scratch and every other work file under `$RUNNER_TEMP/vm-export`. If a capsule container could not be proven stopped, `purge` first force-removes exactly that container, and deletes nothing while Docker still lists it.

The job shares the concurrency group `vm-templates` with `vm-templates.yml` and `k8s-node-templates.yml`: they use the same lab network and token, so only one of them runs at a time. GitHub keeps one pending run per group: an export waiting behind a template build is cancelled when another run of the group queues behind it (the weekly or a push-triggered template build). Dispatch it again.

### Cold-boot state

What a cold boot created on the lab (two clones, one import file) is known only from its state file. The runner empties its temp directory after every job, so that file does not live there: each run keeps it in `$HOME/.proxcloud-vm-export-coldboot/<run id>-<attempt>` on the export runner. `recover` runs `vm_export.coldboot cleanup` on every directory it finds there, this run's and any earlier run's, and deletes a directory only when its cleanup is recorded clean (or when its run never wrote a state file, which means it changed nothing on the lab). Otherwise the directory stays and the step fails.

So an unclean cleanup (a PVE error during a destroy, a pending change the code did not expect, a cancel with too little time, a runner that died) is retried by the next export before it touches the lab, and that export stops there if the lab still cannot be cleaned. Without this, the stranded clones would keep the two test addresses and every later export would run for about 50 minutes and then refuse `test_address_in_use`. A state directory that never gets clean needs an operator: read the cleanup result in it, remove what is left on the lab, then delete the directory.

### Upstream checksum

The manifest names the vendor image the template was built from (`upstream_checksum`, `upstream_url`). Neither template builder records it on the template, so the export derives it and then bounds it:

- **Kubernetes:** the installer ISO pinned in `images/k8s-build-config.py` at the export commit.
- **Native:** the checksum the vendor publishes at export time. `images/build.py` verifies the vendor image against it at build time and keeps one download per OS, named by the first 12 hex of that checksum; the template must be the newest published one of its OS.

In both cases the node must hold the vendor file of that name, and the file must not be newer than the template (the file's `ctime` in the storage listing against `ctime` in the template's `meta`, with five minutes of clock difference allowed). A pin that moved after the template was built, or a vendor image a later build downloaded (even a build that then failed), refuses the export instead of being attributed to the template: `upstream_image_is_not_the_built_one`, `upstream_image_newer_than_the_template` or `template_not_newest`. **Rebuild the template first, then export it.** `ubuntu-2404` follows the vendor's moving `current` image, so this happens whenever the vendor published a new image since the last build. This is a bound, not a proof: a vendor file that was already on the node when the template was built from another one would pass. Recording the checksum on the template in the two builders would close it.

### Masked values and job outputs

GitHub does not pass on a job output that contains a value masked in the job that set it; the job stays green with only a warning. The export job's outputs are three sha256 values (manifest, disk, evidence record) and the artifact name, and every secret of the job is masked. A short secret that happens to occur in one of them would therefore leave the publisher with an empty input after a multi-hour export. Three things prevent that:

- The cold-boot VLAN is a **variable** (`EXPORT_VLAN`), not a secret. A two or three digit secret would be found in a hash or in the name in a large share of runs. The workflow does not reference `secrets.PVE_BUILD_VLAN`.
- `config` refuses any secret of the job that consists of 16 or fewer hexadecimal characters, or that occurs in the artifact name (`config_<NAME>_would_hide_a_job_output`), before any work.
- The publisher's first step requires all four outputs before it does anything else, and names the cause when one arrives empty. Nothing was published; dispatch again after renaming the offending value.

The job's log and summary are masked, the uploaded evidence record is not. The record is therefore not printed in the export job's summary: a masked copy next to the unmasked one would show where a masked value occurs in public data. The hosted `verify` job prints it.

## Trust model

**The export runner is trusted for the content of the image.** It reads the template, runs the sanitizer and decides what bytes are in `disk.qcow2`; the hashes the other jobs check are the ones it computed. A compromised export runner could therefore publish a bad image through this workflow, with a valid attestation. What this workflow does to limit it:

- The export job runs this repository's code at the dispatched commit on `main`, started by someone who may dispatch workflows here. This workflow has no pull-request, push or fork-reachable trigger.
- The job holds no registry credential and no `packages` permission. It cannot push, replace or delete a package, and it cannot sign an attestation. It can only hand a bundle to the hosted publisher of the same run, and the publisher accepts it only for what was dispatched: the variant, OS and Kubernetes version of the form, this commit, this run, and a build time between the run's creation and now.
- The guest never runs on it. Every parser of guest data runs in a container with no network, no capabilities, a read-only root and read-only inputs, and the parent proves the container stopped before it trusts any output.
- The PVE token is the least-privilege image token. It is in the environment of five steps only (configuration check, the two cleanups, copy, cold boot).

**What this workflow cannot do: keep other workflows off that machine.** The statements above describe this file, not the runner. The runner is a persistent machine, its user is in the `docker` group (which is root on that machine), the unsanitized template disks are mounted on it and it reaches the PVE API. Runner labels route jobs; they authorize nothing. Unless the owner sets what follows, this is possible:

- A pull request from a fork changes `runs-on` in any workflow to the four export labels. Its code then runs on the export runner, and whatever it leaves behind owns the next export: the PVE token, the disk content and the hashes the publisher trusts.
- Anyone who can push a branch adds a workflow that targets the runner, reads the repository-level `PVE_*` secrets, or asks for `packages: write` and `id-token: write` and pushes and attests on its own.

**Preconditions the owner must set** (none of them can be expressed in this repository; the first real run does not prove them):

1. **Runner scope.** Register the export runner in an organization runner group that is restricted to this repository **and to the workflow `mutex-solutions/proxcloud/.github/workflows/vm-artifact-export.yml@refs/heads/main`**. No other workflow, branch or fork can then get a job onto it. Do the same for the template build runner and its two workflows.
2. **Fork pull requests.** Require approval for the workflows of **all** outside contributors (repository settings, Actions, "Fork pull request workflows"), not only first-time ones.
3. **`main`.** Protect the branch: pull request with review required, no direct or force push, and the review rule covers `.github/workflows/` and `images/` (code owners).
4. **Secrets in an environment.** Create the environment `vm-artifact-export`, limit its deployment branches to `main`, and move the lab secrets into it (the export job names that environment; until they are moved it falls back to the repository secrets of the same names). A workflow on another branch then cannot read them. The template workflows read the same `PVE_*` secrets at repository level; they need the same treatment (an environment of their own) before the repository-level copies can be deleted.
5. **Package writers.** Only workflows on `main` should be able to write the VM packages: in each package's settings, grant Actions access to this repository only, and rely on 3 for which code runs there. A second writer can still move a tag; consumers use digests.

Two more things a reader of the trust model should know:

- **Caches.** A job on `main` holds the Actions runtime token, which can write the cache of `main`. `docker.yml` restores a `type=gha` cache in the jobs that publish product images. A compromised export or template runner could therefore reach those builds through the cache. This exposure is shared with the template runners and is not new; 1 to 3 are what keep the runner trustworthy.
- **What the attestation says about the runner.** The provenance is signed by the hosted `attest` job, so it names a GitHub-hosted runner, and `gh attestation verify --deny-self-hosted-runners` passes. The disk was nevertheless produced on the self-hosted export runner. The attestation proves which workflow, commit and run pushed the digest, not where the bytes were made.

**The hosted jobs are trusted for what is published and for the statements about it.** They run on fresh GitHub-hosted machines, check the bundle without parsing the image, and refuse to replace a tag. The attestation says: this digest was pushed by this workflow, at this commit, in this run. It does not say the image is clean; that claim rests on the export code, on the evidence record and on the preconditions above.

**Why the publisher is called from the same run.** Two designs were possible: (a) the export run calls the publisher as a reusable workflow, or (b) a separate publisher starts when an export run completes (`workflow_run`). This repository uses (a):

- Producer and publisher are one run at one commit by construction. With `workflow_run` the publisher runs the default branch's code as it is at that moment, which can be a different commit from the export's; requiring equality would refuse every export during which `main` moved.
- The hashes reach the publisher as job outputs of the same run. A `workflow_run` publisher would have to read them from an artifact, that is, from the very upload it is supposed to check.
- The run id and the attempt are GitHub's (`github.run_id`, `github.run_attempt`), not inputs. Nobody can point the publisher at another run.
- The attestation is signed inside the run that exported the image, so its provenance names the export workflow and the dispatch.

The cost of (a) is that the run is still in progress when the publisher looks at it, so "the run completed with success" cannot be the rule. The rule is "the export job of this run attempt concluded with success", read from the API.

## Producer checks

The publisher has no manual trigger. Before it downloads anything, [`images/vm_publish_stage.py`](<../images/vm_publish_stage.py>) requires, from fixed API routes of this repository:

1. The caller is `.github/workflows/vm-artifact-export.yml` on `refs/heads/main` (`github.workflow_ref`, which in a called workflow names the caller), and the run's workflow id and path are that workflow's.
2. The run is this run and this attempt, a `workflow_dispatch` on `main` in this repository (not a fork's head repository), still `in_progress` with no conclusion, at **exactly the commit the publisher checked out**.
3. The artifact name stands for **what was dispatched**: the `variant`, `os` and `kubernetes_version` of the form reach the publisher directly, not through the export runner. Its 12 hex are the first 12 of this commit, and its build time lies between the run's creation and now (five minutes of clock difference allowed). The export runner cannot choose another package, version or tag time.
4. The jobs of this attempt (one page, all of them) contain exactly one job named `export`. It concluded `success`, on the export runner's labels, in this run, at this commit. When only the failed jobs of a run are re-run, the kept export job reports its earlier attempt number; that is accepted, a later one is not.
5. The run has **exactly one artifact with the exact bundle name**: not expired, a bounded size, the expected download route, and the same run, repository and commit in its own metadata. The evidence artifact has another name and is never taken for a bundle.
6. The downloaded files hash to the values the export job recorded as its outputs, and the manifest says the same as the dispatch: variant, OS, Kubernetes version, build time, `source_commit` equal to this commit and `run_ref` equal to this run.

The download route is built by the workflow; a supplied URL is never fetched.

## Public evidence

The `export` job uploads `<bundle name>-evidence`: **one JSON file and nothing else**, `vm-export-evidence.json`. [`images/vm_export/evidence.py`](<../images/vm_export/evidence.py>) builds it from an allowlist. It holds:

- the run: repository, source commit, run id and attempt, run URL, variant, OS, Kubernetes version, build version, artifact name, image tag and registry reference;
- the tool image reference and the local ID that ran;
- the source: sha256 and size of the private copy, the run-scoped source identity, the upstream checksum and URL, and the copy guard results;
- for each capsule (inventory, sanitize, postscan): its check id, exit codes, the stop and cleanup proof, and the sha256 of every file it was given (the copy, the sanitizer, the probes, the review);
- the review: the sha256 of the derived review, of the package policy and of the committed baseline (native: the eligibility result). The review file itself is not in the record: it is the `review` section of the committed baseline in `images/vm_export/baselines/` plus the Kubernetes version, the source sha256 and the package policy sha256, all of which are public, so anyone can rebuild it and compare the sha256;
- the sanitizer checks, the postscan result, the cold-boot checks of both clones, the identity comparison and the cleanup result;
- the hashes of `manifest.json` and `disk.qcow2`.

It never holds a filesystem path, a node, storage or pool name, a VMID, an IP or MAC address, a token id, a container id or guest data. The inventory itself (package and account names read from the guest) stays private; only its sha256 is published. Names that could carry anything are not free text: the tag, the reference and the check ids must equal the values derived from the validated inputs, and a test fails if any other key, or a string that looks like an address or a path, gets in.

The export job reports the sha256 of the record. The hosted `attest` job creates a build-provenance attestation for that sha256, and the hosted `verify` job downloads the artifact, refuses it unless it has that sha256 and names this publication (reference, run, commit and the two file hashes), and prints it in the final summary.

The artifact is kept 90 days and the job summary as long as the run's logs. After that the file is no longer served by GitHub, but a copy stays verifiable for good: `gh attestation verify vm-export-evidence.json` with the flags [below](#how-anyone-verifies-an-image) proves that a kept copy is the record of that run. The record is not attached to the OCI artifact (no `oras attach`), so keep a copy if you need it later. The other lasting records are the image attestation and the manifest inside the OCI artifact.

The sanitized disk is not byte-reproducible: two exports of the same template give two different `disk.qcow2` hashes with the same checks (three runs over the 1.37.2 template gave three hashes). Compare an image with its own run's evidence, not with another run's.

## What the owner sets

**Repository and organization settings.** The five [preconditions of the trust model](#trust-model): the runner group restricted to this workflow on `main`, approval for every outside contributor's fork pull request, protection of `main`, the environment `vm-artifact-export` limited to `main`, and package write access for this repository only.

**Runner.** One self-hosted runner with the labels `self-hosted`, `linux`, `x64` and `proxcloud-vm-artifact-export`. Its user is not root, has a uid and gid of 1000 or higher and may use Docker. It needs Docker with runc and cgroup v2, `ssh` and `ssh-keygen`, the PVE directory storage of the templates mounted **read-only**, a route to the PVE API and to the two cold-boot test addresses, and about 70 GB of free disk under the runner's temp directory. The runner service must have `HOME` set to the user's home directory, a plain path (letters, digits, `/ . _ -`) that survives between jobs: the [cold-boot state](#cold-boot-state) lives there. Remove the old label `proxcloud-vm-artifact-stage`; no workflow uses it.

**Secrets**, in the environment `vm-artifact-export` (or, until they are moved, at repository level, where the first nine already exist for the template builds):

| Secret | Use |
|---|---|
| `PVE_URL`, `PVE_TOKEN_ID`, `PVE_TOKEN_SECRET` | The least-privilege image token |
| `PVE_NODES` | **Exactly one** node name: the node that holds the template and runs the cold boot. A space-separated list is refused |
| `PVE_TEMPLATE_STORAGE` | The storage of the template disks; the cold boot uploads the sealed disk to it and creates the clones on it |
| `PVE_IMPORT_STORAGE` | Native exports only (a Kubernetes export does not need it): the storage where the builder keeps the downloaded vendor image |
| `PVE_ISO_STORAGE` | Kubernetes exports only, optional: the storage that holds the installer ISO. Falls back to `PVE_TEMPLATE_STORAGE`, as in the node template build |
| `PVE_BRIDGE` | The bridge of the cold-boot network |
| `BUILD_GATEWAY`, `BUILD_NAMESERVER` | Gateway and DNS of that network (DNS falls back to the gateway) |
| `EXPORT_TEST_ADDRESSES` | **New.** Two unused addresses in CIDR form on that network, comma separated, reserved for this job, for example `192.0.2.50/24,192.0.2.51/24` |

None of these may consist of 16 or fewer hexadecimal characters or occur in the artifact name (`vm`, `export`, `k8s`, `ubuntu`, `2604`, ...): `config` refuses such a value, see [masked outputs](#masked-values-and-job-outputs). A node or storage called `cafe` or `a1` has to be renamed, or the export cannot run.

**Variables**, in the same environment (or at repository level):

| Variable | Use |
|---|---|
| `EXPORT_VLAN` | **New.** The VLAN of the cold-boot network, 1 to 4094; unset or `0` means untagged. Set it to the value of the secret `PVE_BUILD_VLAN` of the template builds. It must be a variable: as a secret it would be masked and would hide job outputs. As an environment variable it is still not readable from a fork pull request |
| `EXPORT_SOURCE_MOUNT` | Optional. The read-only mount of the template storage on the runner. Default `/srv/pc-ci-images` |

`PVE_BUILD_VLAN`, `BUILD_IP` and `BUILD_DHCP_RANGE` are not used by this workflow. The clone VMIDs 9290 to 9299 are reserved for the cold boot; a template in that range is refused.

**Packages.** Do not count on a new package being private: on this organization a package created by a first push was public at once. The bytes are then readable before the `attest` job has run, so an image is released only when the whole run is green. If a first push does create a private package, the `verify` job fails at the anonymous pull; an owner sets the package to public in its settings and re-runs the failed job, and nothing is pushed again. The tool image package `proxcloud-vm-export-tools` must be public too: the export job pulls it without any credential.

## Export tool image

The export steps in [`images/vm_export/`](<../images/vm_export/>) run inside containers started from one tool image: `qemu-img`, Python with the libguestfs binding, and the kernel and packages libguestfs assembles its appliance from. Like every public artifact, the tools that produce one are built by GitHub Actions from this repository, never by hand.

- **Recipe.** [`images/vm_export/tools/Containerfile`](<../images/vm_export/tools/Containerfile>): a Debian 12 base pinned by digest, and every package from one `snapshot.debian.org` timestamp, so a rebuild resolves the same package versions (the image is not bit-for-bit reproducible: file times and the generated initrd differ). The resolved set is committed as [`packages.lock`](<../images/vm_export/tools/packages.lock>) (`dpkg-query --show` of the image) and every build must reproduce it exactly. The snapshot is fetched over plain http with expired `Release` files accepted; apt still verifies signatures and hashes, and the lock is what stops an older signed index served in its place. To take security updates, move the snapshot timestamp and the base digest in a pull request and replace `packages.lock` with the `packages.txt` of that pull request's `vm-export-tools-reports-<commit>` artifact, after reading the difference. The image has no account and no `sudo`, and no file in it keeps a setuid or setgid bit or a file capability. It is usable as any non-root numeric uid, with `HOME` and `TMPDIR` taken from the environment; `capsule.py` runs it as the invoking runner user (never root) with no capabilities.
- **Build.** [`vm-export-tools.yml`](<../.github/workflows/vm-export-tools.yml>) runs on a hosted runner when the recipe or the workflow changes. It builds the image, compares its package list with `packages.lock`, runs [`smoke.sh`](<../images/vm_export/tools/smoke.sh>) (uid 1002 without a passwd entry, no network, read-only root: no setuid, setgid or capability file, import `guestfs`, run `qemu-img`, launch the TCG appliance, make and check a filesystem), then scans it with the Trivy release and the gate of `security-scan.yml`. A pull request stops there, with a read-only token.
- **Publish.** On `main` the same run then pushes `ghcr.io/mutex-solutions/proxcloud-vm-export-tools:<first 16 hex of the Containerfile SHA-256>-<commit SHA>`. There is no `latest` and no moving tag, and the run refuses to push when the tag already exists. It reads the manifest back from the registry, checks that it is the image that passed the smoke and the scan, and records the reference (`…@sha256:…`, the manifest digest), the config digest and the image ID the hosted runner's Docker reported in the job summary and in the `vm-export-tools-digest-<commit>` artifact. "Never replaced" is a property of this workflow, not of the registry: a GHCR tag is mutable, and any job in this repository that holds `packages: write` could overwrite or delete it. If a run fails after the push (registry read-back, artifact upload, cancellation), the tag exists without a digest record and a re-run for that commit is refused; publish again from a new commit on `main`, because the commit SHA is part of the tag. A tag without the digest record of a successful run is never pinned. The first publication, to a package that does not exist yet, has not been exercised: if GHCR answers the tag probe with anything other than a 404 `MANIFEST_UNKNOWN` or `NAME_UNKNOWN`, the run stops before pushing and the workflow needs a change. The export job pulls the image **anonymously** (an empty Docker client configuration, by design: the tools are the ones anyone can pull), so the package must be public; giving the runner read access would not help.
- **Pin.** [`capsule.py`](<../images/vm_export/capsule.py>) names the image twice. `IMAGE_REFERENCE` is the registry reference `ghcr.io/mutex-solutions/proxcloud-vm-export-tools@sha256:d3ab36a5…` (the manifest digest recorded by the `vm-export-tools` run on `main` at `83ebd33`). `IMAGE_IDS` are the two local IDs that image can have: the same manifest digest on a Docker with the containerd image store (the export runner), and its config digest `sha256:df1770b2…` on a Docker with the classic store. The export job first runs `docker pull` on the reference, with an empty Docker client configuration so no stored registry credential is sent; a capsule itself still starts with `--pull=never`, from the local ID, and refuses any ID outside those two. The image that was built on a workstation and loaded by hand is no longer accepted. A new tool image is used only after a reviewed change of both constants, taken from the `vm-export-tools-digest-<commit>` artifact or the job summary of a successful run on `main`, never from the tag or the registry UI.
- **Scan policy.** The gate is the repository's, unchanged: a HIGH or CRITICAL finding with a fixed version blocks, and findings without a fix are tracked until the re-review date in `check_scan_report.py`, then block. The image carries QEMU, a kernel and the libguestfs appliance packages, which have many findings without a fix in Debian 12, so from that date this workflow cannot publish until the owner decides. The image never receives network input; it parses untrusted disk images under TCG without a network. That is context for the decision, not an exception in the gate.

## Download, extraction and verification

The hosted workflow downloads only the authorized Actions artifact through a bounded HTTPS request (size and timeout limits). It does **not** use automatic `gh run download`, `unzip` or a download action that extracts first and validates paths afterward.

Before the stdlib ZIP parser can allocate its directory, the helper bounds the EOCD/ZIP64 entry count to three and central-directory bytes to 64 KiB, refusing split archives, comments and inconsistent offsets. It then checks the full ZIP member set **before any payload writes**. Exactly the three fixed, unique basenames are accepted. Directories, symlinks/special modes, traversal/absolute/nested names, NUL-name changes, encrypted entries, unsupported compression and out-of-bound sizes fail closed. Stored/deflated members are supported with a conservative compression-ratio bound; the producer should use compression level zero. Streamed extraction enforces member lengths and CRC, and creates an absent destination directory and each regular file exclusively with restrictive permissions. Archive size and aggregate extracted size are bounded. It never calls `extract`/`extractall`, derives output paths from a manifest or overwrites an existing destination.

The [artifact verifier](<../images/vm_artifact.py>) is then called with **both required trusted pins**, and called again immediately before push. It checks strict manifest/receipt schema, source-derived tag, disk length/hash and exact checksum-file bytes; the publisher additionally binds OS/build/source prefix to the artifact name and refuses extra files. No `qemu`, guest boot, cloud-init, image parser, installer or staged script is executed on the package-write runner. This byte verification does **not** prove QCOW2 validity, guest cleanliness, upstream authenticity or bootability.

## OCI upload and no-overwrite policy

The official ORAS setup action is pinned to `22ce207df3b08e061f537244349aac6ae1d214f6` (official v1 revision supplied by independent owner review), requesting ORAS **1.3.0**. Checkout is also pinned to an immutable action revision. The tests are source-only: they are not proof of a successful hosted run.

Only the job's short-lived GitHub token logs into GHCR via stdin; no PAT or PVE secret is requested. Package and unique tag come only from the verified manifest using the existing helper's plan. Native packages are `ghcr.io/mutex-solutions/proxcloud-vm-<os>`; tags are `<os>-os-<UTC build>-<12 source hex>`. The Kubernetes package is `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s` with `ubuntu-2604-k8s-<version>-<UTC build>-<12 source hex>` tags. `<UTC build>` is the export run's own time to the second, so exporting the same template again gives a new tag. There is no `latest`, mutable channel, caller tag or overwrite flag.

Before upload, the publisher checks the exact manifest tag through the fixed GHCR API. **Only HTTP 404 with an explicit `MANIFEST_UNKNOWN`/`NAME_UNKNOWN` registry error permits a first push.** Existing tags, authentication/authorization errors, timeouts, malformed responses and other failures stop publication. This serializes this workflow but does not make GHCR tags immutable against another authorized writer racing outside it; restrict other package writers and use immutable digests for consumers.

ORAS executes the helper's parsed argv without a shell, with its working directory set to the verified extraction, not the checkout root. Only the three explicit filenames/media types are passed. Upload uses image-spec `v1.0`, artifact type `application/vnd.proxcloud.vm-template.v1+json`, and fixed source plus manifest revision annotations. No directory glob, raw staging input or private receipt is uploaded. GHCR upload-size/time limitations still apply even below this contract's disk bound.

The digest is the one of **this push**: ORAS writes the manifest it pushed to a file (`oras push --export-manifest`), and the publisher takes the sha256 of those bytes after checking that the manifest names the two pinned files. It never asks the registry what the tag resolves to afterwards, so another writer who moves the tag during the push cannot change which digest is attested; the `verify` job then pulls exactly that digest. The package and the digest become job outputs for the `attest` and `verify` jobs, and the job summary records the reference and the two file hashes. Registry login is removed in an always cleanup step. A failed push can leave an uploaded but unaccepted artifact; review it rather than bypassing the no-overwrite guard on retry.

## Attestation

The `attest` job runs `actions/attest-build-provenance` (pinned by commit) for the package and the digest the publisher recorded. It is the only job with `id-token: write` and `attestations: write`. The attestation is stored by GitHub and also pushed to the registry next to the image. It binds the digest to this repository, the export workflow, the commit and the run.

The job then runs `gh attestation verify` on that digest with exactly the flags the final summary prints, and fails if it does not verify: a run is green only when the command anyone will use succeeds. A second attestation covers the sha256 of the [evidence record](#public-evidence).

The attestation is created after the push, so an image whose `attest` job failed is published without one: treat such an image as not released and export again. The signer is the hosted `attest` job; see [what the attestation says about the runner](#trust-model).

## How anyone verifies an image

The final job summary prints these commands with the real values. With the [GitHub CLI](https://cli.github.com/), [ORAS](https://oras.land/) and a checkout of this repository:

```text
gh attestation verify oci://ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s@sha256:<digest> --repo mutex-solutions/proxcloud --signer-workflow mutex-solutions/proxcloud/.github/workflows/vm-artifact-export.yml --source-ref refs/heads/main
oras pull --output pull ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s@sha256:<digest>
python3 images/vm_artifact.py verify --staging-dir "$PWD/pull" --expected-manifest-sha256 <manifest sha256> --expected-disk-sha256 <disk sha256>
gh attestation verify vm-export-evidence.json --repo mutex-solutions/proxcloud --signer-workflow mutex-solutions/proxcloud/.github/workflows/vm-artifact-export.yml --source-ref refs/heads/main
```

1. `gh attestation verify` proves the digest was pushed by the export workflow as it is on `main`. **Keep `--signer-workflow` and `--source-ref`.** With `--repo` alone the command also accepts an attestation made by any workflow on any branch of this repository, including a changed copy of the export workflow. Add `--source-digest <commit>` to require the commit the summary names.
2. `oras pull` by digest needs no login. Pull by digest, not by tag: a GHCR tag is mutable.
3. `vm_artifact.py verify` checks the manifest schema, the tag derived from it, the disk length and hash and the exact `SHA256SUMS` bytes against the two hashes of the run summary and of the evidence record.
4. The last command checks a copy of the evidence record (from the final summary or from the run's `<bundle name>-evidence` artifact) against the attestation of its sha256.

The QCOW2 is for a reviewed hypervisor import, **not `docker run`**. The byte verification does not prove that the image boots; the cold boot of the export run is the boot evidence.

## What only a real run can confirm

- The GitHub API lists an artifact, and serves its archive, while the run that uploaded it is still in progress, and it accepts the `name` filter on a run's artifact list as an exact match (the evidence artifact's name starts with the bundle's).
- The jobs API names the export job `export` in a run that also calls a reusable workflow and whose export job uses an environment, and reports its `labels` as the four `runs-on` labels. The run object carries `created_at` in the form `2026-11-14T12:00:00Z`.
- `github.workflow_ref` in the called publisher is the export workflow's ref, and a job output of the export job reaches the hosted jobs (see [masked outputs](#masked-values-and-job-outputs): the guard covers the secrets, not a mask GitHub adds on its own).
- `oras push --export-manifest` (ORAS 1.3.0) writes the manifest it pushed, with one layer per file whose `digest` is the file's sha256, and GHCR serves the sha256 of those bytes as the artifact's digest.
- The attestation action accepts an ORAS artifact as its subject and can push to GHCR with the job token; `gh attestation verify oci://…` with `--signer-workflow` and `--source-ref` finds and accepts the attestation right after it was created, with the `gh` version of the hosted runner; a subject given only as a name and a sha256 (the evidence record) is accepted.
- `actions/download-artifact` in the `verify` job places `vm-export-evidence.json` directly in the given directory.
- The image token may read `/nodes/<node>/tasks`, the pool, and the content listing of the import storage (native) and of the ISO storage (Kubernetes). Those listings carry an integer `ctime` for each file, and the template's configuration carries `meta` with `ctime=<seconds>`; the upstream bound refuses with `upstream_image_time_unknown` otherwise.
- The runner's temp directory has no symlink in its path (the capsule refuses a bind source that is not its own real path) and is disk-backed. `HOME` is set for the runner's jobs and is a plain path.
- A cancel leaves enough time for the cleanup step. When it does not, or when the runner dies during the cold boot, the next export's `recover` step finds the state and cleans up; that path has only run against fakes.
- The sanitizer finishes inside its 60-minute limit on the export runner (it took about 13 minutes on a workstation; it runs under TCG, so the runner's single-core speed decides).
- Native exports: the copy of a real QCOW2 template file (larger than 20 GiB) through inventory and the sanitizer in the workflow; the vendor checksum published at export time is the one of the image the node holds.
- `GET .../pending` lists no pending change for a clone configured while stopped (see [public VM sanitization](<public-vm-sanitization.md>)); if it does, the cold boot ends unclean and `recover` keeps its state.
- None of the [owner preconditions](#trust-model) is checked by a run.

## Tests

[`test_vm_export_workflow.py`](<../images/tests/test_vm_export_workflow.py>) checks the workflow text: the exact top-level keys (no workflow-wide environment), triggers, inputs, permissions per job, pinned actions, the order of the steps, where secrets may appear, the artifact names, the attestation verification command and that the anonymous job has no token. [`test_vm_export_pipeline.py`](<../images/tests/test_vm_export_pipeline.py>) runs the driver with fakes for the capsules, the cold boot and the PVE client, and the real copy guards, bundle and evidence code; it compares every stage's output and the job summary with the exact expected text. [`test_vm_publish_workflow.py`](<../images/tests/test_vm_publish_workflow.py>) covers the producer checks, the ZIP guards and the reusable publisher. None of them contacts GitHub, a registry or PVE.

```text
python3 -m unittest discover -s images/tests
```
