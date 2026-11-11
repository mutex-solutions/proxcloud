# Public VM artifact contract v1

**Delivery target: actual portable QCOW2 VM disks on public GHCR**, not only a site's Proxmox templates, container images, or a manifest-only release. This change defines the pure local contract; it does **not** export/sanitize a VM, wire a pipeline, upload anything, or prove a public artifact exists. The project pipeline will add those steps separately.

## Fixed staging and trust boundary

The stdlib [helper](<../images/vm_artifact.py>) accepts `prepare|verify --staging-dir /absolute/operator/staging`. Use a dedicated, reviewed Linux directory with no concurrent writer and no untrusted writable ancestors. It reads only fixed names: `disk.qcow2`, `request.json`, `clean-export-receipt.json`; prepare exclusively creates `manifest.json` and `SHA256SUMS` (use a fresh directory for retry). Verify needs only the three public payload files. Symlinks, hardlinks, special files and metadata over 64 KiB are refused; outputs never follow manifest-supplied paths. It never imports the existing builder, reads environment secrets, opens an image parser, invokes a command or accesses the network.

`request.json` contains exactly `kind: "vm-template"`, `os` (`ubuntu-2404`, `ubuntu-2604`, `debian-12`, `debian-13`), `arch: "amd64"`, `variant: "os"|"kubernetes"`, lowercase 40-hex `source_commit`, lowercase 64/128-hex official `upstream_checksum`, and UTC `build_version` as real-calendar `YYYYMMDDHHMMSS`. Kubernetes requires Ubuntu and numeric `kubernetes_version` such as `1.35.10` (major 1..999, minor/patch 0..999; no `v` prefix/prerelease). Optional `provenance` contains only `upstream_url`, HTTPS on official Ubuntu/Debian image hosts, without credentials/query/fragment/traversal. Official-host syntax does not validate remote content or a matching OS release.

A **trusted reviewed exporter**, not a caller checkbox, must supply the separate `clean-export-receipt.json`: exactly `kind: "CleanExportReceipt"`, opaque `source_identity: "sha256:<64 lowercase hex>"`, matching `source_commit`, public `run_ref` (`https://github.com/mutex-solutions/proxcloud/actions/runs/<positive id>`), `request_sha256`, `disk_sha256`, `disk_bytes`, `virtual_size_bytes`, and `checks`. Request hash is SHA256 of the helper's `encode(request)` canonical bytes (sorted keys, two-space indent, ASCII, final newline). Identity must hash a non-secret run-scoped source identifier, never a password/token or a bare predictable VMID. No private node/VMID/user/IP/path/pool fields or arbitrary notes are allowed.

The exact check keys must all be JSON `true`: `cleaned_copy_only`, `no_backing_file`, `qemu_img_check`, `offline_sanitization`, `zero_free_space`, `no_build_credentials_or_lab_network`. The exporter must actually complete them in its separately reviewed isolation boundary and bind the receipt to these exact disk bytes and request. Missing/false/unknown facts refuse preparation. **The unsigned receipt is an assertion to review, not cryptographic attestation, a sanitizer, or proof of absence of secrets.** A malicious caller can fabricate it; this helper alone never grants publication eligibility.

The manifest adds `schema_version: 1`, derived `image_tag`, `disk` (fixed `filename: "disk.qcow2"`, `format: "qcow2"`, actual `sha256`, actual positive `bytes`, receipt `virtual_size_bytes`), and the validated `clean_export_receipt`. Duplicate/unknown JSON keys are refused. Disk bytes must be **strictly below 8 GiB**; virtual size must be **20..100 GiB inclusive**, independently supplied by the reviewed exporter. Hashing opaque bytes does not validate QCOW2 format, backing-file absence, virtual size, sanitization, bootability or upstream authenticity. Synthetic tests intentionally demonstrate that distinction.

## Identity, verification and ORAS plan

Tags derive from OS + `os`/`k8s-<numeric version>` + UTC timestamp + 12-hex source prefix, e.g. `ubuntu-2604-os-20261111000000-aaaaaaaaaaaa`. No `latest`/`dev`, user tag override or implicit overwrite. This is a collision-resistant naming convention, not a registry immutability guarantee: reject existing tags in the future publisher and pin full OCI digests for consumers. OS packages use `ghcr.io/mutex-solutions/proxcloud-vm-<os>`; Kubernetes packages add `-k8s`.

`SHA256SUMS` has exactly two lines, for `disk.qcow2` and `manifest.json` (never its own self-hash). Verify requires schema/receipt consistency and actual disk/hash/length matches; optional `--expected-manifest-sha256` and `--expected-disk-sha256` accept literal lowercase 64-hex trusted pins. Unpinned hashes detect corruption, not malicious replacement; **no cosign/signature verification is implemented or claimed**. A full OCI package digest is independent of these file hashes and must be verified by the registry client against a trusted release reference.

Both commands print a JSON **plan only**: ORAS push argv using image-spec `v1.0`, artifact type `application/vnd.proxcloud.vm-template.v1+json` and fixed payload media types (`application/x-qemu-disk`, `application/vnd.proxcloud.vm-template.manifest.v1+json`, `text/plain`). Only these three named payloads are included, never a directory/glob or private staging inputs. No shell interpolation or ORAS execution occurs. `execute` and `publish_eligible` remain false; separate trust-review, roundtrip-boot and explicit-public/anonymous-pull release gates remain false, not inferred from a locally valid hash.

## Public release and end-user retrieval (future operator actions; not executed)

GHCR supports generic OCI artifacts/ORAS. Its documented **10 GB per layer / 10 minute upload** bounds apply; this contract's <8 GiB disk leaves size headroom but does not guarantee an upload can finish. See [GHCR container-registry guidance](<https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry>) and [ORAS push](<https://oras.land/docs/commands/oras_push/>). A newly published package is private by default even if its source repository is public. An authorized owner must explicitly change package visibility to **public** in package settings; public visibility is irreversible. Do not claim delivery from a successful authenticated push alone.

Before public release, review exporter evidence and scan the disk, upload only the intended three-file payload to quarantine/private storage, pull it by OCI digest into a clean environment, verify trusted file pins, import/boot an isolated full-copy VM and accept cloud-init/SSH, resize, guest-agent, sanitization and (if applicable) Kubernetes tests. Then explicitly set public visibility and prove the exact digest can be pulled **anonymously with no ambient registry credentials**. Record digest/file pins and boot acceptance separately; a first upload may be needed before roundtrip testing, but upload is not public release. No broker, pipeline or these runtime gates is implemented by this helper.

After that future release, an end user would run (replace the digest with the trusted published OCI digest):

```text
oras pull ghcr.io/mutex-solutions/proxcloud-vm-ubuntu-2604@sha256:<full-64-hex-OCI-digest>
python3 images/vm_artifact.py verify --staging-dir /absolute/isolated/pull --expected-manifest-sha256 <trusted-64-hex-file-hash> --expected-disk-sha256 <trusted-64-hex-disk-hash>
```

Use an empty nonprivileged pull directory and a reviewed ORAS client: this helper cannot make extraction of arbitrary hostile OCI payloads safe. Pull by digest only (no ambiguous tag+digest syntax); the downloaded disk is for reviewed hypervisor import, **not `docker run`**. The verification command does not import or boot it.

## Source-only tests and outstanding gates

[Unittest tests](<../images/tests/test_vm_artifact.py>) use synthetic QCOW magic/header-like bytes and temporary files only; intended command: `python3 -m unittest discover -s images/tests -p test_vm_artifact.py -v`. **Not run for this change.** No image build/export, qemu tool, PVE action, network, package installation, CI dispatch, registry publication, visibility change, anonymous pull, real boot or signature check has been performed. Trusted exporter/sanitizer implementation, pipeline integration and all release gates remain separate work.
