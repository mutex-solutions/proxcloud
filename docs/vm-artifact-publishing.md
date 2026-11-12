# VM artifact publishing bridge

## Status and scope

This is **source-only publication plumbing**, not evidence of a published/public disk. No commands, tests, network calls, image exports, PVE operations, Actions runs, registry pushes, visibility changes or boot acceptance were executed for this change. The [publisher](<../.github/workflows/vm-artifacts.yml>) handles actual QCOW2 **bytes** only after an independently reviewed exporter has produced a sealed, sanitized bundle; it never fabricates an empty/example disk to satisfy a release gate.

The bridge accepts native OS exports (`variant: os`) for `ubuntu-2404`, `ubuntu-2604`, `debian-12` and `debian-13`, and **only** the accepted Kubernetes node template line (`variant: kubernetes`, `os: ubuntu-2604`, `kubernetes_version: 1.35.10`) under a separate artifact name. The variant is derived from the artifact name, never a free input. A native name never admits a Kubernetes manifest, the reverse is also refused, and any other Kubernetes OS or version is refused. Kubernetes bundles publish to `ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604-k8s` with tags `ubuntu-2604-k8s-1.35.10-<UTC build>-<12 hex>`. Their disk must come from the root-only `--k8s-os` sanitizer path ([sanitization boundary](<public-vm-sanitization.md>)), which never exports the template's EFI vars or cloud-init media and preserves kubelet enablement. Kubernetes runtime acceptance is still required: actual inventory, sanitize, fresh postscan, cold boot via fresh cloud-init and anonymous full pull. No OS choice, package override, URL, command, tag, VMID, node, path, deployment, registry credential or PVE credential is a dispatch input. No PVE rights are added. Existing template builders, provider pins and tenant VMs are unchanged.

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
