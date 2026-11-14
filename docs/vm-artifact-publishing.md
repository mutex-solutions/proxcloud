# VM artifact publishing bridge

## Status and scope

This is **source-only publication plumbing**, not evidence of a published/public disk. No commands, tests, network calls, image exports, PVE operations, Actions runs, registry pushes, visibility changes or boot acceptance were executed for this change. The [publisher](<../.github/workflows/vm-artifacts.yml>) handles actual QCOW2 **bytes** only after an independently reviewed exporter has produced a sealed, sanitized bundle; it never fabricates an empty/example disk to satisfy a release gate.

The bridge accepts native OS exports (`variant: os`) for `ubuntu-2404`, `ubuntu-2604`, `debian-12` and `debian-13`, and **only** the Kubernetes node template lines (`variant: kubernetes`, `os: ubuntu-2604`, `kubernetes_version` one of `1.35.10`, `1.36.6`, `1.37.2`: the explicit allowlist `K8S_VERSIONS`, the same in the publisher, the sanitizer and the copy policy) under a separate artifact name. The variant is derived from the artifact name, never a free input. A native name never admits a Kubernetes manifest, the reverse is also refused, and any other Kubernetes OS or version is refused (adding a version is a reviewed change to the three allowlists, kept equal by `images/tests/test_k8s_versions.py`). Kubernetes bundles publish to `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s` with tags `ubuntu-2604-k8s-<version>-<UTC build>-<12 hex>`. Their disk must come from the root-only `--k8s-os` sanitizer path ([sanitization boundary](<public-vm-sanitization.md>)), which never exports the template's EFI vars or cloud-init media and preserves kubelet enablement. Kubernetes runtime acceptance is still required: actual inventory, sanitize, fresh postscan, cold boot via fresh cloud-init and anonymous full pull. No OS choice, package override, URL, command, tag, VMID, node, path, deployment, registry credential or PVE credential is a dispatch input. No PVE rights are added. Existing template builders, provider pins and tenant VMs are unchanged.

**Publication record (2026-11-13):** one Kubernetes artifact has been published through this bridge: `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s:ubuntu-2604-k8s-1.35.10-20261112032843-8a2a737b331b` (2026-11-12; its tag list is readable anonymously). It is a separately dispatched artifact, not part of the app chart, the installer BOM or any app release tag. Publication is not node acceptance: no kubeadm join or NodeReady is claimed, and the runtime checks in the [sanitization boundary](<public-vm-sanitization.md>) remain the acceptance gate.

## Authority before bytes

The publisher is manual `workflow_dispatch` only, restricted to `mutex-solutions/proxcloud` on `refs/heads/main`, on an ephemeral GitHub-hosted Ubuntu 24.04 runner. It checks out exactly `github.sha`, with no persistent checkout credential. Only `contents: read`, `actions: read` and `packages: write` are granted to the job. No PR/fork trigger, self-hosted runner, environment deployment, release trigger or matrix is present.

Required inputs are strings, checked before checkout/API access and again by the [staging helper](<../images/vm_publish_stage.py>):

| Input | Required form and source |
|---|---|
| `staging_run_id` | Positive decimal run ID, 1–20 digits; no leading zero |
| `artifact_name` | Exactly `vm-export-<supported-os>-<YYYYMMDDHHMMSS>-<12 lowercase hex>`, or `vm-k8s-export-ubuntu-2604-<YYYYMMDDHHMMSS>-<12 lowercase hex>` for the Kubernetes variant; timestamp must be a real UTC calendar date/time |
| `expected_manifest_sha256` | Literal 64 lowercase hex from independent trusted review; no `sha256:` prefix |
| `expected_disk_sha256` | Literal 64 lowercase hex from independent trusted review; no `sha256:` prefix |

Hashes copied from an untrusted payload are not independent trust pins. Maintainer dispatch authority and the fixed producer workflow are the trust boundary; the unsigned receipt and hashes are **not** sanitization or cryptographic attestation.

The API origin/repository/producer path are source constants. The publisher looks up the active workflow ID for `.github/workflows/vm-artifact-stage.yml`; that separately reviewed staging workflow must exist before publication can work. It then requires:

1. The selected run belongs to that exact workflow ID/path and to the same original repository, not a fork's head repository.
2. Event `workflow_dispatch`, status `completed`, conclusion `success`, head branch `main`, and producer head SHA **exactly equal to the publisher's checkout SHA**. A source-only module/workflow revision requires a new staging run; an old staging run cannot silently use a newer verifier.
3. The run lists **exactly one artifact**, with the exact requested name, a positive bounded ID/size and `expired: false`. Multiple artifacts, duplicate-name ambiguity, pagination totals, expired payloads and mismatched embedded producer-run metadata fail closed.
4. The advertised archive URL exactly matches GitHub's expected same-repository API route for that artifact ID. The workflow constructs that route itself; it never downloads a supplied URL. GitHub may redirect that route to its signed HTTPS storage URL; authorization is not forwarded to a different host.

The producer's head SHA binds its workflow/helper revision, **not** the original VM build's source revision. The latter remains the complete `source_commit` in the manifest, is bound to the artifact-name suffix and is retained in the OCI revision annotation. Do not rewrite build provenance to make those two revisions look identical.

## Staging producer contract (separate implementation/review)

The future/independently implemented `.github/workflows/vm-artifact-stage.yml` may only read a fixed operator-owned sealed staging location, verify its pins and upload the three public payload files. Its authority, fixed source directory, unprivileged temporary runner label, local filesystem ownership and no-concurrent-writer policy require separate review. It must not gain PVE credentials, guest execution, arbitrary source-path/URL/command inputs or package-write permissions. An artifact upload from an arbitrary successful repository workflow is **not** accepted.

Exporter/copy/sanitizer work is outside this bridge. It must act only on a trusted offline **copy**, independently prove image integrity/backing-file absence and sanitized credentials/network state, and emit the [reviewed receipt and bundle contract](<public-vm-artifacts.md>). Neither this publisher nor a fabricated receipt makes the original live PVE disk safe to publish. Private source paths, topology, VM identifiers and detailed inspection evidence do not belong in public bundles/logs.

Upload a flat artifact named as above, ideally with `actions/upload-artifact` `compression-level: 0`, containing exactly:

- `disk.qcow2`: positive length strictly below 8 GiB;
- `manifest.json`: at most 64 KiB;
- `SHA256SUMS`: at most 64 KiB.

No parent directory, dotfile, request sidecar, raw receipt sidecar, raw export log, private recovery file or unrelated artifact is allowed. The sanitized receipt is already embedded in the manifest. The staging workflow must complete successfully before its run is selected. Staging/upload is not public release and must not auto-trigger publication.

## Export tool image

The export steps in [`images/vm_export/`](<../images/vm_export/>) run inside containers started from one tool image: `qemu-img`, Python with the libguestfs binding, and the kernel and packages libguestfs assembles its appliance from. Like every public artifact, the tools that produce one are built by GitHub Actions from this repository, never by hand.

- **Recipe.** [`images/vm_export/tools/Containerfile`](<../images/vm_export/tools/Containerfile>): a Debian 12 base pinned by digest, and every package from one `snapshot.debian.org` timestamp, so a rebuild resolves the same package versions (the image is not bit-for-bit reproducible: file times and the generated initrd differ). The resolved set is committed as [`packages.lock`](<../images/vm_export/tools/packages.lock>) (`dpkg-query --show` of the image) and every build must reproduce it exactly. The snapshot is fetched over plain http with expired `Release` files accepted; apt still verifies signatures and hashes, and the lock is what stops an older signed index served in its place. To take security updates, move the snapshot timestamp and the base digest in a pull request and replace `packages.lock` with the `packages.txt` of that pull request's `vm-export-tools-reports-<commit>` artifact, after reading the difference. The image has no account and no `sudo`, and no file in it keeps a setuid or setgid bit or a file capability. It is usable as any non-root numeric uid, with `HOME` and `TMPDIR` taken from the environment; `capsule.py` currently runs it as `1000:1000` with no capabilities.
- **Build.** [`vm-export-tools.yml`](<../.github/workflows/vm-export-tools.yml>) runs on a hosted runner when the recipe or the workflow changes. It builds the image, compares its package list with `packages.lock`, runs [`smoke.sh`](<../images/vm_export/tools/smoke.sh>) (uid 1002 without a passwd entry, no network, read-only root: no setuid, setgid or capability file, import `guestfs`, run `qemu-img`, launch the TCG appliance, make and check a filesystem), then scans it with the Trivy release and the gate of `security-scan.yml`. A pull request stops there, with a read-only token.
- **Publish.** On `main` the same run then pushes `ghcr.io/mutex-solutions/proxcloud-vm-export-tools:<first 16 hex of the Containerfile SHA-256>-<commit SHA>`. There is no `latest` and no moving tag, and the run refuses to push when the tag already exists. It reads the manifest back from the registry, checks that it is the image that passed the smoke and the scan, and records the reference (`…@sha256:…`, the manifest digest), the config digest and the image ID the hosted runner's Docker reported in the job summary and in the `vm-export-tools-digest-<commit>` artifact. "Never replaced" is a property of this workflow, not of the registry: a GHCR tag is mutable, and any job in this repository that holds `packages: write` could overwrite or delete it. If a run fails after the push (registry read-back, artifact upload, cancellation), the tag exists without a digest record and a re-run for that commit is refused; publish again from a new commit on `main`, because the commit SHA is part of the tag. A tag without the digest record of a successful run is never pinned. The first publication, to a package that does not exist yet, has not been exercised: if GHCR answers the tag probe with anything other than a 404 `MANIFEST_UNKNOWN` or `NAME_UNKNOWN`, the run stops before pushing and the workflow needs a change. A new GHCR package is private, so the owner must give the export runner read access to it before the pin below can work.
- **Pin.** [`capsule.py`](<../images/vm_export/capsule.py>) starts containers only from the image ID in its `IMAGE` constant, with `--pull=never`. Today that ID is an image built on a workstation and loaded onto the runner by hand. That image is transitional: after the first published build, a reviewed follow-up change sets `IMAGE` to the published image and the runner pulls it by digest. That reviewed digest is the control, so it comes only from the `vm-export-tools-digest-<commit>` artifact or the job summary of a successful run on `main`, never from the tag or the registry UI. `IMAGE` is what `docker image inspect --format '{{.Id}}'` prints on the export runner after `docker pull <reference>`: the record's `config_digest` on a Docker with the classic image store, the digest in `reference` with the containerd image store (the record's `image_id` shows which one the hosted runner reported). The reviewer checks that it is one of those two recorded values. Until that change merges, nothing uses the published image.
- **Scan policy.** The gate is the repository's, unchanged: a HIGH or CRITICAL finding with a fixed version blocks, and findings without a fix are tracked until the re-review date in `check_scan_report.py`, then block. The image carries QEMU, a kernel and the libguestfs appliance packages, which have many findings without a fix in Debian 12, so from that date this workflow cannot publish until the owner decides. The image never receives network input; it parses untrusted disk images under TCG without a network. That is context for the decision, not an exception in the gate.

## Download, extraction and verification

The hosted workflow downloads only the authorized Actions artifact through a bounded HTTPS request (size and timeout limits). It does **not** use automatic `gh run download`, `unzip` or a download action that extracts first and validates paths afterward.

Before the stdlib ZIP parser can allocate its directory, the helper bounds the EOCD/ZIP64 entry count to three and central-directory bytes to 64 KiB, refusing split archives, comments and inconsistent offsets. It then checks the full ZIP member set **before any payload writes**. Exactly the three fixed, unique basenames are accepted. Directories, symlinks/special modes, traversal/absolute/nested names, NUL-name changes, encrypted entries, unsupported compression and out-of-bound sizes fail closed. Stored/deflated members are supported with a conservative compression-ratio bound; the producer should use compression level zero. Streamed extraction enforces member lengths and CRC, and creates an absent destination directory and each regular file exclusively with restrictive permissions. Archive size and aggregate extracted size are bounded. It never calls `extract`/`extractall`, derives output paths from a manifest or overwrites an existing destination.

The [artifact verifier](<../images/vm_artifact.py>) is then called with **both required trusted pins**, and called again immediately before push. It checks strict manifest/receipt schema, source-derived tag, disk length/hash and exact checksum-file bytes; the bridge additionally binds OS/build/source prefix to the selected artifact name and refuses extra files. No `qemu`, guest boot, cloud-init, image parser, installer or staged script is executed on the package-write runner. This byte verification does **not** prove QCOW2 validity, guest cleanliness, upstream authenticity or bootability.

## OCI upload and no-overwrite policy

The official ORAS setup action is pinned to `22ce207df3b08e061f537244349aac6ae1d214f6` (official v1 revision supplied by independent owner review), requesting ORAS **1.3.0**. Checkout is also pinned to an immutable action revision. Action/source/tool integrity and first live operation still require owner review; source-only tests are not proof of a successful hosted setup.

Only the job's short-lived GitHub token logs into GHCR via stdin; no PAT or PVE secret is requested. Package and unique tag come only from the verified manifest using the existing helper's plan. Native packages are `ghcr.io/mutex-solutions/proxcloud-vm-<os>`; tags are `<os>-os-<UTC build>-<12 source hex>`. The Kubernetes package is `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s` with `ubuntu-2604-k8s-1.35.10-<UTC build>-<12 source hex>` tags. There is no `latest`, mutable channel, caller tag or overwrite flag.

Before upload, the publisher checks the exact manifest tag through the fixed GHCR API. **Only HTTP 404 with an explicit `MANIFEST_UNKNOWN`/`NAME_UNKNOWN` registry error permits a first push.** Existing tags, authentication/authorization errors, timeouts, malformed responses and other failures stop publication. This serializes this workflow but does not make GHCR tags immutable against another authorized writer racing outside it; restrict other package writers and use immutable digests for consumers.

ORAS executes the helper's parsed argv without a shell, with its working directory set to the verified extraction, not the checkout root. Only the three explicit filenames/media types are passed. Upload uses image-spec `v1.0`, artifact type `application/vnd.proxcloud.vm-template.v1+json`, and fixed source plus manifest revision annotations. No directory glob, raw staging input or private receipt is uploaded. GHCR upload-size/time limitations still apply even below this contract's disk bound.

After upload, ORAS resolves the exact unique tag to a full `sha256:<64 hex>` OCI digest. The job summary records only the package/digest, trusted manifest/disk hashes, a digest-pinned pull command and outstanding acceptance gates. Detailed arbitrary disk/receipt/metadata data is not printed as a publication receipt. Registry login is removed in an always cleanup step. A failed push/resolve can leave an uploaded but unaccepted artifact; review it rather than bypassing the no-overwrite guard on retry.

## Public visibility and real end-user pull remain explicit

A successful upload is **not** a public release. A newly created GHCR package is private by default, even from a public repository; an already-public package remains public, so review its visibility **before** uploading. To keep candidate bytes private for roundtrip acceptance, do the first upload while that package is still private. An owner must explicitly make the intended package **public** in package settings after review. Public visibility can be irreversible; the publisher does not call an undocumented visibility API or claim the setting was changed.

Before announcing delivery:

1. Independently review sealed exporter/sanitizer evidence and trusted file pins.
2. Pull the exact OCI digest into an isolated environment, verify both file pins and import/boot a full-copy VM. Accept cloud-init/SSH, guest-agent, growth/capacity, sanitation and the OS-specific gates. No original template/tenant/provider pin may be modified as a shortcut.
3. Have an authorized owner explicitly approve/set package public visibility.
4. Pull that same digest **anonymously**, with no registry auth file, ambient Docker/ORAS credential helper, token or logged-in user. Record the public digest and verified file pins, not only a successful authenticated push.

Only after those gates would an end user retrieve a released native image with:

```text
oras pull ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604@sha256:<trusted-full-64-hex-OCI-digest>
python3 images/vm_artifact.py verify --staging-dir /absolute/isolated/pull --expected-manifest-sha256 <trusted-manifest-file-hash> --expected-disk-sha256 <trusted-disk-file-hash>
```

The QCOW2 is for reviewed hypervisor import, **not `docker run`**. A byte-verifier result is not a boot result. No public download, anonymous pull, roundtrip boot or signature verification is claimed by this source change.

## Pure offline regression coverage

The [new tests](<../images/tests/test_vm_publish_workflow.py>) contain synthetic API metadata, non-QCOW2 sample bytes and temporary ZIPs/directories only. They cover strict inputs, producer/path/repository/event/conclusion/revision binding, exact artifact selection, ZIP path/type/duplicate/encryption/size/compression guards, pinned file validation, source-derived package/argv, existing-tag refusal and static hosted workflow constraints. They intentionally never run commands, contact APIs/registries, obtain credentials, parse/boot an image or mutate a VM.

Intended future owner-authorized execution: `python3 -m unittest discover -s images/tests -p test_vm_publish_workflow.py -v`. **Not run here**. Workflow syntax/runtime, staging authority, actual export/sanitization, first private upload, roundtrip acceptance and anonymous public retrieval remain separately reviewed/executed gates.
