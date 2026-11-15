# Review of HIGH/CRITICAL findings without a fix (2026-11-15)

The `security-scan` gate ([`check_scan_report.py`](../.github/scripts/check_scan_report.py)) blocks on
any HIGH or CRITICAL finding with a fixed version. Findings with no fixed version are tracked, not
blocking, until `UNFIXED_REVIEW_BY`; from that UTC date on they block again until someone reviews
them. This file is that review for the date 2026-12-12.

**Outcome.** Owner decision of 2026-11-15, after this review: `UNFIXED_REVIEW_BY` moves by 90 days,
from 2026-12-12 to **2027-03-12**. Nothing else in the gate changes: no ignore list, no allowlist, no
new VEX. The single owner VEX (CVE-2025-47273, expires 2026-12-31) is not part of this decision and
did not move; see [The VEX date](#the-vex-date-not-part-of-this-decision).

This review records what an outsider's data can reach. It is not a claim that any image is free of
exploitable flaws.

## Sources

| | security-scan | vm-export-tools |
|---|---|---|
| Run | 43665338716, on `main` | 43573036256, on `main` |
| Commit | `13276f5fa7824a12bc89c93ad8413eb6b5371b2a` | `d3a43515e91bb03d2ce7892c5d78816ac19ccb4e` |
| Scanned | 2026-11-15, 01:55 to 02:08 UTC | 2026-11-14, 07:53 UTC |
| Trivy | 0.76.2 | 0.76.2 |
| Trivy DB `UpdatedAt` | 2026-11-14T19:00:12Z | 2026-11-14T01:05:40Z |
| Trivy DB `DownloadedAt` | 2026-11-15T01:55:48Z to 01:58:42Z | 2026-11-14T07:52:43Z |

Artifacts used: `security-<target>-13276f5…` (product images, `filesystem`, controlled-base prototype),
`security-template-images-13276f5…` (`templates/<name>/scan.json`, `template-status.json`),
`security-base-images-13276f5…` (report-only; of its thirteen bases only `external-python`, the default
`python` template, is reviewed here) and
`vm-export-tools-reports-d3a4351…` (`scan.json`, `packages.txt`). The counts below were recomputed from
each `scan.json` (HIGH/CRITICAL, empty `FixedVersion`); they equal the gate's "tracked" count for
every target. The Debian/Ubuntu status is Trivy's `Status` field from that DB: `affected` (no fixed
package yet), `fix_deferred` (the distribution has postponed the fix) and `will_not_fix`. The Debian
security tracker was not consulted directly.

## Which targets block

- **Blocking** (final shipped targets): the source lockfiles (`filesystem`), the six product images
  (`api`, `web-client`, `web-admin`, `sandbox-proxy`, `sandbox-runtime`, `sandbox-import`), the eleven
  non-GPU sandbox templates, the controlled-base prototype, and the VM export tool image (same CLI, in
  `vm-export-tools.yml`).
- **Report-only**: the thirteen raw upstream bases (`scan_base_images.py`). One of them,
  `external-python`, is not only a base: it is the exact digest of the default `python` sandbox
  template, which sites run as is (see [Default `python` template](#default-python-template)). None of
  its findings blocks, with or without a fixed version, before or after 2027-03-12.
- **Not scanned**: the GPU/CUDA template `code-interpreter-gpu`.

## Per-target counts

Unfixed = HIGH/CRITICAL without a fixed version, counted per binary package (one CVE in four binaries
of one source package counts four times, as the gate counts).

| Target | OS | Unfixed (CRITICAL) | affected | fix_deferred | will_not_fix | Unique CVEs | Source packages carrying most findings |
|---|---|---:|---:|---:|---:|---:|---|
| `filesystem` and the six product images | — / Alpine 3.24.4 / Debian 12.15, 13.7 | 0 | 0 | 0 | 0 | 0 | none: no HIGH/CRITICAL at all, with or without a fix |
| `template/base`, `amp`, `claude`, `codex`, `gemini`, `mcp-gateway`, `opencode` (each; identical sets) | Debian 12.15 | 650 (22) | 514 | 126 | 10 | 329 | linux 193 (headers), krb5 140, openexr 46, glib2.0 35, util-linux 33, mariadb 32 |
| `template/node` | Debian 12.15 | 640 (20) | 508 | 122 | 10 | 323 | as `base`, without gh and bluez |
| `template/code-interpreter` | Debian 13.7 | 395 (11) | 380 | 12 | 3 | 102 | krb5 140, mariadb 56, util-linux 48, python3.13 20, linux 18, libxml2 16 |
| `template/browser` | Ubuntu 24.04 | 2 (0) | 2 | 0 | 0 | 1 | gst-plugins-bad1.0 2 (CVE-2025-3887) |
| `template/desktop` | Ubuntu 22.04 | 0 | 0 | 0 | 0 | 0 | none |
| `controlled-base-prototype` | Debian 13.7 | 152 (1) | 148 | 4 | 0 | 61 | krb5 56, util-linux 36, linux 18, openssh 6, git 6, sqlite3 6 |
| VM export tool image | Debian 12.15 | 372 (8) | 300 | 70 | 2 | 134 | qemu 172 (43 CVEs), krb5 56, util-linux 36, curl 15, glib2.0 7, libxml2 7 |
| `python` template (raw base `external-python`; **report-only**, never blocks) | Debian 13.7 | 47 (0) | 46 | 1 | 0 | 11 | util-linux 36, ncurses 4, sqlite3 3, systemd 2, acl 1, perl 1; plus 2 HIGH **with** a fixed version (zlib) |

Across all blocking targets (every row except `python`): 6,111 tracked findings, 413 unique CVEs. All
of them are in OS packages; no language package (npm, PyPI, Go) has an unfixed HIGH/CRITICAL finding.

## Reachability

### Sandbox templates

**What they are and where they run.** The built-in templates of the managed sandboxes
(`deploy/templates/<name>/Dockerfile`; [managed-sandbox.md](managed-sandbox.md) §5.4, §6.3, §9).
Sites pull them by the digests in `deploy/templates/images.yaml` and pre-pull them on the sandbox
workers. A sandbox runs the **tenant's own code** under gVisor (`runsc`, platform `systrap`), so the
code talks to gVisor's kernel, not to a Linux kernel from the image. The user `user` has passwordless
sudo, and with `allow_suid` on (the default) any process in a sandbox can become root of that sandbox
(§1.4). Egress reaches the internet by default (private and link-local ranges and TCP 25 denied;
`allow_internet_access=False` blocks it). What a sandbox holds belongs to its tenant: their files,
envd's access token, the MCP servers' keys in `mcp-gateway` (§9.2), the agent CLIs' credentials in
`claude`, `codex`, `gemini`, `amp` and `opencode`.

**The boundary.** The tenant already runs arbitrary code as root inside the sandbox, so a flaw in a
package the tenant can run gives that tenant nothing new. What matters is a package that parses data
from a **third party** inside the sandbox (a web page, a repository, a document, a server the
tenant's code or agent connects to): a flaw there can hand that third party the tenant's sandbox. The
sandbox boundary itself (gVisor, the guardrail, the worker firewall) is not in these images.

I sorted the findings by source package into three groups, using the package's role and each CVE's
title (not the code):

| Group | `base` and the five agent templates and `mcp-gateway` | `node` | `code-interpreter` | prototype |
|---|---|---|---|---|
| A. No code that runs in a sandbox: `linux`, `krb5`, `mariadb`, `postgresql-15`, `bluez`, `gnupg2` | 377 findings / 221 CVEs | 369 / 217 | 229 / 51 | 74 / 32 |
| B. Local privilege only: `util-linux`, `sudo`, `pam`, `acl`, `systemd`, `ncurses` | 49 / 8 | 49 / 8 | 58 / 8 | 44 / 8 |
| C. Network clients and parsers of third-party data | 224 / 100 | 222 / 98 | 108 / 43 | 34 / 21 |

- **A.** `linux` is `linux-libc-dev`, kernel headers, no kernel code (193 of 650 findings in the
  Debian 12 templates). The kernel of the sandbox workers is outside these images and outside this
  scan. The krb5 CVEs describe the KDC and kadmind, the MariaDB and PostgreSQL ones the servers and
  server tools, the BlueZ ones the AVRCP/A2DP stack, the GnuPG one `tpm2daemon`; the templates carry
  only client libraries and headers of those packages (`libkrb5-3`, `libmariadb3`, `libpq5`,
  `libbluetooth3`, their `-dev` packages). Not checked against the code one by one.
- **B.** sudo, PAM, mount/nsenter, libacl, systemd-homed, ncurses: privilege inside the sandbox, which
  the tenant already has.
- **C.** These are the findings that can matter to a tenant. Reached when the tenant's code or agent
  downloads, clones, connects to or opens something another party controls:
  - network clients: curl/libcurl (5 CVEs on Debian 12: connection reuse, cookie and proxy-auth
    leaks, TLS settings, SFTP host verification), wget (2: heap overflow from a server-supplied
    filename, crafted HTML), git (3, one is code execution through submodule metadata in a recursive
    clone),
    openssh-client (6, among them CRITICAL CVE-2026-60002, a client-side use-after-free during host
    key re-exchange; one title is about sshd, which is not installed), and in the seven templates
    built on `e2bdev/base` Debian's gh 2.23.0 (CRITICAL CVE-2024-52308 and CVE-2026-48501);
  - parsers of images, documents and archives: OpenEXR (23 CVEs, 3 CRITICAL), libheif (16), libtiff
    (4), ImageMagick (1), librsvg (1), aom (2, one CRITICAL), harfbuzz (1), libxml2 (7, one CRITICAL),
    expat (4), SQLite (6, one CRITICAL; code execution from a crafted database file), zlib (3, the
    CRITICAL minizip one is `will_not_fix`), gzip (LZH), Perl Archive::Tar, the Debian Python
    packages (tarfile, expat; on Debian 13 also the HTML parser and TLS certificate checks), GLib
    (GVariant, key files, regex, D-Bus introspection), libX11/libXrender (a hostile X server),
    OpenLDAP;
  - `browser` (Playwright, Ubuntu 24.04): its only finding, CVE-2025-3887 in GStreamer's H.265 parser,
    may be reached by a web page with H.265 media when the tenant drives WebKit, which uses the system
    GStreamer. Not verified.

  None of these has a fixed package in Debian or Ubuntu as of the DB date. Every template build runs
  `apt-get upgrade`, so a distribution fix reaches the next build (this does not cover the `python`
  template, which is never built; see below); a finding that gains a fix while
  an image still carries the old version blocks at the next scan. Most of group C in the Debian 12
  templates comes from what `e2bdev/base` and `node:bookworm` install (image libraries, `-dev`
  packages, Debian's gh), not from what the templates add; the controlled-base prototype, which drops
  most of that, has 34 findings in group C instead of 224.

### Controlled-base prototype

`deploy/templates/base/Dockerfile.controlled`: a nondefault candidate parent for the base template
(`python:3.11-slim-trixie`, gh 2.103.3 built from source, Node 22). Only `security-scan` builds it; it
is not in `build.sh` or `images.yaml`, never published, and no site runs it. No untrusted input
reaches it today. If it becomes the base, it runs as a sandbox template with the boundary above. Its
group C is curl (2), wget (2), git (3), openssh-client (6, including CVE-2026-60002), expat (4),
SQLite (3) and Perl Archive::Tar (1).

### Default `python` template

**What and where.** The only template in the installer defaults
(`installer/playbooks/group_vars/all/defaults.yml`: `python: {image: "pin:python", …}`).
`deploy/templates/images.yaml` pins it to `docker.io/library/python:3.12-slim@sha256:05a994d2…`, a
public image used as is ([managed-sandbox.md](managed-sandbox.md) §6.3). It runs tenant code with the
same boundary as the other templates. This project never rebuilds it, so the `apt-get upgrade`
argument above does not apply: a Debian fix reaches sites only when someone moves the pin.

**Gate.** `security-scan` scans that exact digest only as the raw base `external-python`
(`scan_base_images.py`, "external runtime (not PR rebuild)"), which is report-only. Its `gate.txt`
says `Blocking HIGH/CRITICAL: 2; tracked without fix …: 47`, but neither number fails the job, today
or after 2027-03-12. The review date and the VEX do not change anything for this image.

**Findings** (`bases/external-python/scan.json`, Debian 13.7):

- **2 HIGH with a fixed version**, both in `zlib1g` 1:1.3.dfsg+really1.3.1-1+b1, fixed in
  1:1.3.dfsg+really1.3.1-1+deb13u1: CVE-2026-85091 (title: heap buffer overflow in `gz_vacate()`
  allows arbitrary code execution) and CVE-2026-76844 (title in this DB is about webpack-dev-middleware;
  in the tool image's DB it is a zlib heap buffer overflow through a stale `gzwrite` pointer). The
  rebuilt Debian 13 templates (`code-interpreter`, the prototype) do not report them, because
  `apt-get upgrade` installs the fixed package. Not checked whether the Python runtime or a tenant's
  code reaches the `gz*` file functions.
- **47 without a fixed version**: 11 CVEs, 0 CRITICAL, 46 `affected`, 1 `fix_deferred`. Group B
  (local privilege only): 43 findings / 7 CVEs in util-linux (mount helpers, nsenter), systemd-homed,
  libacl and ncurses. Group C: 4 / 4, SQLite (3: a crafted FTS5 index, crafted session changesets, an
  FTS5 heap overflow) and Perl Archive::Tar (1: a crafted tar header). Nothing in group A.

So the only default template carries two HIGH zlib findings with a fixed version, and no job fails
because of them. Unlike the other templates, the scanned digest here is the pinned one itself.

### VM export tool image

**What and where.** `images/vm_export/tools/Containerfile`: Debian 12.15, every package from
`snapshot.debian.org` at `20261114T000000Z`, with `qemu-utils`, `python3-guestfs` (libguestfs,
`qemu-system-x86` and the appliance packages), `supermin` and the kernel `linux-image-6.1.0-54-amd64`
6.1.190. It runs only as capsules (`images/vm_export/capsule.py`) on the self-hosted export runner,
in `vm-artifact-export.yml`, which is a manual dispatch on `main`. Each capsule is
`docker create --pull=never --network=none --read-only --cap-drop=ALL --security-opt=no-new-privileges`,
as a non-root host uid/gid of at least 1000, with pids, memory, CPU, open-file and file-size limits
and a wall clock, only the declared read-only input binds plus one disk-backed scratch bind and a
`/tmp` tmpfs. The image has no setuid, setgid or file-capability file (`smoke.sh` checks). libguestfs
runs its appliance as a TCG guest (`force_tcg`, no `/dev/kvm`).

**Input.** A read-only copy of a lab template disk that this project built (template guard, copy
receipt with the disk's sha256). [vm-artifact-publishing.md](vm-artifact-publishing.md) still treats
it as an untrusted disk image.

**Reach.**

- Network code (curl 15 findings, wget, openssh-client, krb5, libslirp, spice-server, GStreamer's
  RTCP parser, libvirt's RPC handler): a capsule has no network, so no peer reaches them.
- The disk is parsed by `qemu-img` in the capsule, and its partitions and filesystems by the appliance
  kernel and tools inside the TCG guest (for example the libblkid DOS partition parser,
  CVE-2026-53615). Of the 43 qemu CVEs, the 19 with a usable title concern device emulation (sdhci,
  9pfs, lsi53c895a, virtio-snd/-gpu/-net/-scsi/-balloon, qxl, VGA, xHCI, Hyper-V, VAPIC), the socket
  I/O channel and the guest agent; none names a disk format driver. The other 24 have no usable title
  in the DB and were not triaged. A device-model flaw needs code running in the appliance guest to drive
  it, so a hostile disk would first have to take over the appliance kernel.
- **The appliance kernel has no findings in this report**: Trivy reported none for it (its source
  package is `linux-signed-amd64`). The headers of the same kernel version (6.1.190) carry 193 open
  findings in the Debian 12 templates. Kernel flaws reachable through a crafted filesystem are
  therefore not counted here.
- A disk that broke through the appliance kernel and QEMU would run as the unprivileged capsule uid,
  without network or capabilities, and could falsify that capsule's own output (inventory, review,
  sanitized disk, postscan). Whoever can put such a disk on the template store already controls the
  template being published; nobody else has an input path.
- Other parsers in the image that no capsule calls directly (gawk, netpbm, sleuthkit, GRUB, capstone,
  json-glib, libtiff, GStreamer): not checked whether supermin copies them into the appliance.

**Which image.** The scanned image is the `d3a4351` build (image ID `sha256:2d7c9557…`, pushed as
`…@sha256:7c5ace58…`). `capsule.py` pins the `83ebd33` build (`sha256:d3ab36a5…`). The recipe and
`packages.lock` are unchanged between the two commits, the scanned `packages.txt` has the same
344 entries as `packages.lock`, and every build must reproduce the lock, so the pinned image has the
same package versions. The pinned digest itself was not scanned.

## Findings with a fixed version in blocking targets

One: **CVE-2025-47273**, setuptools 70.3.0 (fixed in 78.1.1), HIGH, in the `Python` result of ten
targets: the templates `amp`, `base`, `browser`, `claude`, `code-interpreter`, `codex`, `gemini`,
`mcp-gateway`, `opencode` and the controlled-base prototype. The gate classifies it as the owner VEX
(setuptools 70.3.0 is listed only in pip 26.3's vendored SBOM, `pip/_vendor/bom.cdx.json`), which is
why it does not block today. It is not in `node`, `desktop`, the product images or the tool image.
No other finding with a fixed version exists in any blocking target. That holds only for the
blocking set: the default `python` template is outside it and has 2 (zlib; see
[Default `python` template](#default-python-template)). The other twelve raw upstream bases also have
many findings with a fixed version (for example 2,987 in `template-e2b-base`); they are parents,
builders or donors of images that are rebuilt and scanned as blocking targets, and are not reviewed
here.

## The VEX date (not part of this decision)

`OWNER_VEX.expires` is 2026-12-31, and the gate applies the VEX only while the UTC date is before
it. **The last day it applies is 2026-12-30.** From 2026-12-31T00:00Z (one day before 2027-01-01) the
setuptools finding is "fixed version available" and blocks in the ten targets above. Replayed with
this evidence and a database timestamp set to the replay time: `template/base`, `code-interpreter`
and the prototype exit 1 with `Blocking HIGH/CRITICAL: 1`; `node` and the tool image exit 0. The
`template-images` job and the prototype's leg of the `scan` job then fail, so the required
`security-gate` fails on every pull request and every push to `main` until the owner decides. The
VEX also requires pip to be exactly 26.3, so a pip change needs a VEX change too. The tool image is
not affected by the VEX.

## The new review date

From **2027-03-12T00:00Z** every tracked finding blocks again (replayed: `base` 651 blocking,
`node` 640, `code-interpreter` 396, the prototype 153, the tool image 372; all exit 1). Until then
the gate behaves as before, with the new date in its messages. This applies to the blocking targets
only: the `python` template's findings, fixable or not, still do not block after that date.

## What was and was not checked

Checked:

- The counts and statuses of every blocking target and of `external-python` (the `python` template),
  recomputed from each `scan.json` and compared with the gate's output.
- The source package of every finding, and Trivy's title of every unique CVE except those of the
  `linux` source package (kernel headers).
- The recipes and documents of each target named above.
- The tool image's scanned package list against `packages.lock`.
- The gate CLI on three of these reports before and after the date change (same exit codes and
  counts).

Not checked:

- No CVE was traced to the code paths or binaries it affects. The groups above rest on each
  package's role and the CVE title; some titles in the DB are missing or wrong (24 of the 43 qemu
  CVEs; one zlib CVE carries a webpack title in one of the two DBs).
- The Debian and Ubuntu security trackers, beyond the status in the Trivy DB.
- Kernels: the sandbox workers' kernels and the export appliance's kernel.
- The images sites actually run: `images.yaml` pins template builds from `20261112-9d00563`, and
  `capsule.py` pins the `83ebd33` tool image. The gate scans fresh local builds of the current
  sources. The `python` pin is the exception: its digest is the one scanned.
- The GPU/CUDA template (pinned in `images.yaml`, never scanned), the raw upstream bases other than
  `external-python`, and MEDIUM and LOW findings.
- Which interpreter each template's tools use (Debian's `python3.x` or `/usr/local`), whether
  Playwright's WebKit loads the GStreamer H.265 parser, which packages supermin puts into the
  appliance and which devices the appliance configures.

## What the next review must do (by 2027-03-12)

1. Take fresh `security-scan` and `vm-export-tools` runs on `main`, recompute this table, and list
   what changed since 2026-11-15.
2. Go through group C of each template and the tool image's disk path package by package in the
   Debian/Ubuntu tracker: fix, remove the package, or write down why it stays.
3. Decide on the inherited surface: the controlled base instead of `e2bdev/base`, Debian's gh 2.23
   in the seven `e2bdev/base` templates, the image and `-dev` libraries no template needs.
4. For the tool image: move the snapshot timestamp, `packages.lock` and the `capsule.py` pin; check
   the appliance kernel's CVEs separately, since Trivy reports none for `linux-signed-amd64`; triage
   the qemu CVEs without a title.
5. Scan the digests that are pinned (`images.yaml`, `capsule.py`), not only fresh builds, and decide
   on the GPU template.
6. For the default `python` template: move the `images.yaml` pin to a digest that carries zlib
   1:1.3.dfsg+really1.3.1-1+deb13u1 or later, and put that digest under the blocking gate (or build it
   like the other templates), so that its findings can block.
7. Add the result here as a new dated section. `UNFIXED_REVIEW_BY` moves only by a new owner
   decision. The VEX needs its own decision before 2026-12-31.
