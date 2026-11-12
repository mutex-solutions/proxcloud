# Template updates from GitHub Packages

**Admin → System → Template updates** shows the newest VM templates and sandbox images published on
GitHub Packages (`ghcr.io/mutex-solutions`), compares them with what this site runs, and — for VM
templates — imports a chosen artifact as a **new** Proxmox template. Scope chosen by the owner
(2026-11-12): *the admin sees the latest available version and clicks update*. Nothing updates on its
own.

| Part | State |
|---|---|
| GitHub integration (PAT, validated on save) | done |
| Discovery: newest VM artifact per line, newest sandbox image per template, installed/pinned state | done |
| VM import job (download by digest → verify → new template) | done, **off by default** (`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`) until lab-verified |
| Sandbox mirror → prepull → pin switch from the portal | **not yet enabled** — seam + follow-up design below |

Code: [`internal/templateupdates`](../internal/templateupdates), admin routes in
[`internal/platform/admin/templateupdates.go`](../internal/platform/admin/templateupdates.go), page
[`TemplateUpdatesPage.tsx`](../web/admin/src/pages/system/TemplateUpdatesPage.tsx).

## 1. GitHub integration

**System → Integrations → GitHub** stores one token: a **personal access token (classic)** with the
`read:packages` scope. GitHub Packages does not accept fine-grained tokens
([GitHub docs: About permissions for GitHub Packages](https://docs.github.com/en/packages/learn-github-packages/about-permissions-for-github-packages)).

- Saved as a `thirdPartyIntegration` of type `GitHub`, `secret: {"token": "ghp_…"}`. The secret is
  encrypted at rest like every integration secret, is write-only (never returned by any read, list,
  audit diff or log). On update an absent `secret`/`token` keeps the stored one; a `token` key that is
  present must be a non-empty string (400 otherwise), so a blank or non-string value can never wipe it.
- **One GitHub integration**: creating a second answers 409 (edit the existing one). Should several
  exist (created before this check), the oldest by `createdAt` is used, deterministically.
- **Validated before it is stored**: `GET https://api.github.com/user` with the token. A classic token
  reports its scopes in `X-OAuth-Scopes`; the save is refused when that header is absent (fine-grained /
  app token) or does not contain `read:packages` (or `write:`/`delete:packages`, which imply it). A
  rate-limited check answers 429 with the reset time.
- **Every GitHub and GHCR call uses it**: REST calls send `Authorization: Bearer <PAT>`; registry calls
  exchange it (HTTP Basic, only to the realm on the registry's own host) for a pull-scoped registry
  token. There is **no anonymous fallback**: without the integration the page shows *Configure the
  GitHub integration* and the API answers 409.
- The token never enters a job payload, receipt, audit event, error or log line. The client type
  redacts itself under `%v`/`%+v`/`%#v`/slog, transport errors are stripped of URLs, and the
  `Authorization` header is dropped on any cross-host redirect (GHCR blob → signed storage URL).
- Rate limits are surfaced, not hidden: `X-RateLimit-*` of the last call is shown on the page; a
  primary (403/429 + `X-RateLimit-Remaining: 0`) or secondary (`Retry-After`) limit becomes HTTP 429
  with the reset/retry time. Discovery results are cached for 2 minutes; **Check now** bypasses it.

## 2. Discovery (read-only)

`GET /api/v1/admin/template-updates` (`admin:service:read`).

### VM templates

Packages (source constant): `proxcloud-vm-{ubuntu-2404,ubuntu-2604,debian-12,debian-13}` and
`proxcloud-vm-ubuntu-2604-k8s`. For each, `GET /orgs/mutex-solutions/packages/container/<pkg>/versions`
(100 per page, following `Link: rel="next"` by page number — never a URL from the response — up to 20
pages).

- Tags are immutable-style ([public-vm-artifacts.md](public-vm-artifacts.md)):
  `<os>-os-<YYYYMMDDHHMMSS>-<12hex>` or `<os>-k8s-<ver>-<YYYYMMDDHHMMSS>-<12hex>`. There is no
  `latest`; any other tag is ignored. A tag must belong to its package (an `os` tag in the `-k8s`
  package is ignored) and Kubernetes is accepted only on Ubuntu, as in the contract.
- **Update line** = OS + variant (+ Kubernetes version). Lines never supersede each other: 1.36 does
  not replace 1.35.
- **Newest** = highest `build_version` from the tag, then package `created_at`, then digest. The
  newest three per line are content-checked; the first that passes is offered and the others are
  listed as *refused by contract checks* with the reason.
- **Content checks before offering** (by digest only; manifest bytes must hash to the digest):
  - OCI image manifest (not an index), artifact type
    `application/vnd.proxcloud.vm-template.v1+json` (ORAS image-spec v1.0 puts it in
    `config.mediaType`; v1.1 in `artifactType` — both accepted), exactly three layers titled
    `disk.qcow2` (`application/x-qemu-disk`), `manifest.json`
    (`application/vnd.proxcloud.vm-template.manifest.v1+json`) and `SHA256SUMS` (`text/plain`).
  - `manifest.json` passes a Go port of `images/vm_artifact.py validate` (exact key sets at every level,
    duplicate keys refused, integer-only sizes, disk < 8 GiB, virtual size 20–100 GiB, receipt checks
    all `true`, `request_sha256` = SHA-256 of the canonical request, `image_tag` derived). The test
    suite feeds it output generated by the Python reference `prepare` to keep the two in step.
  - `SHA256SUMS` equals exactly the two expected lines.
  - The disk layer's digest equals `sha256:<manifest disk.sha256>` and its size `disk.bytes`.
  - `image_tag` and package match the tag being offered.
- **Installed state** per Proxmox provider (templates in pool `pc-templates`):
  - native OS: `pc-img-<os>` templates; up to date when a template's `pc-v-` version (minute precision)
    is ≥ the artifact's build, or it carries `pc-src-<digest prefix>` of the latest artifact;
  - Kubernetes: pipeline templates (`pc-k8s;pc-k8s-<ver>;pc-k8s-os-<os>`) or ones this feature
    imported (`pc-tu-k8s;pc-k8sver-<ver>`);
  - a provider whose cluster cannot be read is reported as such — never as "nothing installed".
  - *Update available* = a valid newest artifact exists and at least one reachable provider has
    nothing at least as new on that line.

### Sandbox templates

For every built-in name (`deploy/templates/images.yaml`) and every `sandbox-<name>` a sandbox
provider's template uses, the newest `<YYYYMMDD>-<sha7>` tag of `ghcr.io/mutex-solutions/sandbox-<name>`
(by date, then created time). Offered only if its manifest is a container image (OCI/Docker image
manifest with an image config, or an index with a `linux/amd64` image; artifacts refused). *Pinned* =
templates whose image's last path segment is `sandbox-<name>` (the ghcr source or the
`<registry>/library/` mirror), compared by digest. The page offers the exact `images.yaml` pin line.

## 3. VM import job

`POST /api/v1/admin/template-updates/vm` (`admin:service:manage`, audited by the admin audit
middleware with package/tag/digest/target). Body:

```json
{"package":"proxcloud-vm-debian-13","digest":"sha256:…","confirmDigest":"sha256:…",
 "serviceId":"…","node":"pve1","importStorage":"nas-import","diskStorage":"local-zfs","publish":false}
```

**Inputs that the code cannot know are explicit and fail closed**: the provider, node, import
storage and template disk storage are chosen by the admin from
`GET /admin/template-updates/placement/{serviceId}`, which lists per node only the *active* storages
with `import` content and with `images` content (the provider's configured `importStorage` first). A
placement not in that list is refused. The dialog shows the exact OCI digest, disk sha256, sizes and
source commit; `confirmDigest` must repeat the digest.

Enqueue re-lists the package and re-verifies the artifact **server side** (the client's tag/size are
never trusted), then in ONE transaction under an advisory lock on the payload identity: refuses a
duplicate (a receipt for the same provider/node/digest whose step is not `failed` **and** whose durable
job is not `dead`), enqueues the durable job (`templateupdate.vm/v1`, key = `tu-vm-` + SHA-256 of
service/node/digest, so it never exceeds the 256-byte key limit; a retry after a failure gets a
`-<n>` suffix) and creates the receipt — refusing (409) if a receipt for that job id already exists
instead of resetting its progress.

### Download path

The large blob stream uses a separate HTTP client with **no total timeout** (a `http.Client.Timeout`
also bounds the body read, which would kill a multi-GiB download after 30 s): it is bounded by the job
context (2 h per acquisition), the transport's response-header timeout (30 s) and a stall guard (no
byte for 2 minutes aborts it). API, manifest, token and small-blob calls keep a 30 s per-request
deadline.


The API streams the GHCR blob into Proxmox's **upload API** (`POST /nodes/{node}/storage/{s}/upload`,
`content=import`, `checksum=<disk sha256>`, `checksum-algorithm=sha256`) — the same path customer image
import uses (`pve.Client.UploadImportFile`). Bytes are verified three times before a VM is created:

1. a tee SHA-256 + byte counter on the stream, bounded by the manifest's `disk.bytes`: one byte more
   aborts the request body immediately; a short or different body fails the final read, so pveproxy
   never completes the file;
2. Proxmox's own sha256 check of the uploaded file (the upload task fails and PVE keeps no file);
3. the layer digest = `sha256:<disk.sha256>` (checked at discovery and again at enqueue).

`import-from` then also refuses a qcow2 with a backing file. A mismatch is a permanent failure
(`digest_mismatch` / `pve_checksum_refused`).

Rejected alternative: resolving GHCR's short-lived signed blob URL in the API and handing it to PVE
`download-url` with `checksum`/`checksum-algorithm=sha256`. It saves the API's bandwidth, but the node
would then fetch an internet URL itself (PVE follows redirects and re-resolves DNS — the reason
`imageimport.go` never gives PVE a URL), nodes need egress to the storage CDN, the signed URL can expire
mid-download, and the URL (a bearer capability) would land in PVE task logs. Streaming keeps the
nodes off the internet and the verification in our process. The cost: the API's bandwidth and one
upload per provider at a time (pveproxy spools to the API node's root disk; the room check is made
first).

### Steps, receipts and fencing

Durable jobs are DB-only by contract (`durablejobs.Worker` runs handlers inside the ACK transaction),
so this kind runs on a **dedicated runner** and the DB-only worker refuses to register it. New store
primitives: `Checkpoint(job, fn)` commits `fn` only while that acquisition (owner UUID + epoch) still
holds an unexpired lease (rechecked at commit), and `Fail(job, delay, permanent, code)` with a fixed
`[a-z_]` code (never a provider error string). The runner renews the lease every third of it; a refused
renewal cancels the work context.

Receipt (`templateUpdate`, id = job id) steps:
`queued → uploading (intent: file name) → uploaded → creating (intent: VMID) → imported → templated →
succeeded | failed`.

- **Every PVE mutation is immediately preceded by a successful fence check** (no other PVE call in
  between). A lost lease therefore stops the executor before its next effect; it does not complete,
  dead-letter or clean up — the next acquisition owns the job. Tested at every possible loss point.
- **Resume**: a new acquisition continues from the last committed step. `uploaded` is reused while
  the verified file is still in the import store; an intent-only `uploading` (outcome unknown) is
  redone under a fresh, epoch-specific file name, so a superseded runner still uploading can never
  collide; `creating` with a VMID re-reads that VM: ours and imported → continue, ours and empty →
  delete and recreate, someone else's → never touched, pick another VMID.
- **Ownership**: the job tags its *unfinished* VM `pc-tu-<16 hex of job id>` at create time and names
  its files `pc-tu-<…>-e<epoch>-<digest prefix>.qcow2`. A VM is the job's to mutate or destroy only if
  **all** hold: it is a member of pool `pc-templates` on the job's node (read from
  `/pools/pc-templates`), it carries that tag, and it is not `pc-public`. The orphan sweep only
  considers `pc-templates` members. **The finished template does not keep the ownership tag**: PVE
  clones copy tags (CAPMOX keeps them), so neither a clone (always outside `pc-templates`) nor the
  finished template can ever look destroyable; cleanup of a finished receipt only drops import files.
  Only import files with the job prefix are deleted. Existing templates are never modified or deleted.
- **Idempotent finish**: a template that already carries exactly this job's final name and tags (the
  final write landed but its task wait failed) is accepted as done on the next attempt.
- **Failure classes**: an import-from call that fails before PVE answered, or whose task did not
  finish/was unreadable, is retried; only a 400 refusal or a task that finished with a non-OK exit
  status is the permanent `pve_import_refused`.
- **VMID** follows `images/build.py next_vmid`: the lowest free id from 9200 (cluster-wide), retried on
  "already exists".
- **Result**: a new template in pool `pc-templates`, the pipeline's hardware shape
  (`images/build.py`, `pve.ImportImage`; no NIC, cloud-init drive on the disk storage), named
  `pc-ghcr-<os>-<os|k8s-x-y-z>-<build>` (never `pc-<os>-…`, which `build.py` would treat as its own
  failed build) and tagged `proxcloud;pc-src-<12hex>` (no job ownership tag) plus:
  - native OS: `pc-img-<os>;pc-v-<yyyymmddhhmm>` and `pc-public` **only if "publish" was chosen**,
    otherwise `pc-tu-staged` (review it, then add `pc-public`); a published one joins the catalog
    through the existing discovery (`pve.DiscoverTemplates`);
  - Kubernetes: `pc-tu-k8s;pc-k8sver-<x-y-z>`; never a catalog entry — reference its VMID in the
    kamaji provider's version map.
  The description records the artifact reference, tag and disk sha256.
- Retries: 3 attempts with backoff; permanent failures (`digest_mismatch`, `pve_checksum_refused`,
  `pve_import_refused`, `pve_forbidden`, `ownership_lost`, `invalid_payload`, …) dead-letter at once.
- **Dead jobs are cleaned up**: a job can die without running its cleanup (e.g. its last attempt's
  lease expired). When idle, the runner re-acquires one dead, uncleaned job with
  `durablejobs.ClaimDead` — a fenced acquisition (new owner, higher epoch) pinned at
  `attempts = max_attempts`, so it can never become a normal run — runs the same ownership-checked
  `Cleanup`, marks the receipt `cleaned` (`failed`/`job_dead` unless it had finished) and returns the
  job to dead (`cleaned_up`). A provider that cannot be reached leaves it for the next pass.
- **Replay** through `POST /admin/durable-jobs/{id}/replay` (payload re-validated) requires
  `admin:service:manage` for template-update jobs (the billing key `admin:bill:manage` for billing jobs).
- **Upload serialization**: the runner holds a PostgreSQL session advisory lock per
  (provider, node, import storage) for the whole upload, across API replicas; a busy lock retries later.

**Proxmox rights** (the portal token, as for customer image import): `Datastore.AllocateTemplate` on
the import storage (upload), `Sys.Audit` on `/nodes` (upload room check), `VM.Allocate`/`VM.Config.*`
on `/pool/pc-templates` (already granted by `ProxCloudPlatform` on `/pool`) and `Datastore.AllocateSpace`
on the disk storage. Deleting the uploaded file needs `Datastore.Allocate` on the import store; without
it the template is still created and the receipt carries a warning.

### Enabling

`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true` (or `proxcloud.template-updates.vm-enabled`) starts the
runner and allows enqueueing. Off: discovery still works and the import button is disabled. Before
turning it on, run one import per OS on the lab (PVE 9.2) and boot a clone: see *Open items*.

## 4. Sandbox templates — not yet enabled (follow-up design)

`POST /admin/template-updates/sandbox` answers **501** with the manual path. Why: built-in images live
in the sandbox registry's `library/` namespace, writable only by the installer's Zot `admin` account
(kept in `generated/`, never given to the portal), and the prepull DaemonSet is applied by stage 55;
the portal holds no RBAC on it. `internal/registrypins` fences **project** template repositories
(`t/<project>/<template>`); its refs cannot express `library/` and its locks are project rows, so it
cannot be reused as-is for built-ins.

Today's path (unchanged, shown on the page): put the pin line into `deploy/templates/images.yaml`,
`make upgrade` → stage 55 copies it with the same digest and bumps the prepull → the portal's
`sandboxTemplateAdopt` job switches each template once every prepull pod on a Ready node has pulled it
(`SetTemplateImage` CAS; the previous pin stays until then, and stays if the prepull never completes).

Proposed follow-up (needs owner approval as a separate change):

1. **Narrow Zot account** `mirror` created by stage 55 next to `gc`: read/create on
   `library/sandbox-*` only (no delete, no `t/**`, no `library/proxcloud-*`), password generated into
   `generated/` and handed to the portal by stage 60 as `secret.registryMirror` on the sandbox
   provider. Zot's most-specific-pattern rule needs the explicit `library/sandbox-*` entry.
2. **Mirror** = a durable job (external kind, same runner/fence pattern) running the existing
   trusted `sandbox-import` worker pod pattern (`ImportObjects`, fixed argv, digest-pinned worker,
   pod-UID evidence) in copy mode with destination `<registry>/library/sandbox-<name>:<tag>` and
   `--preserve-digests` semantics: source `ghcr.io/mutex-solutions/sandbox-<name>@<digest>` with the
   GitHub integration's token as a short-lived pull secret owned by the pod. Success requires the
   destination to resolve to the **same digest**.
3. **Prepull**: a `Role` in `sandbox-system` allowing `get`/`patch` on `daemonsets` with
   `resourceNames: [template-prepull, template-prepull-gpu]` only, bound to the portal's sandbox
   identity, plus a ValidatingAdmissionPolicy limiting that identity's patches to the init-container
   image list, and only to digest refs under `<registry>/library/sandbox-*`. The job adds/replaces the
   one init container of that image.
4. **Switch** = no new code: the existing adoption job moves templates only after every prepull pod
   on a Ready node runs the new digest. **Rollback** = the previous pin is untouched until adoption;
   reverting is re-patching the DaemonSet back (adoption follows) — the old image stays in `library/`
   because nothing deletes it (Zot GC only removes unreferenced blobs, and the old tag still references
   it).
5. **Pin record**: the mirrored digest is also written to a `sandboxBuiltinPin` document so a later
   `make upgrade` (which renders the DaemonSet from `images.yaml`) does not silently roll it back; the
   installer must read it, or the page must keep showing the `images.yaml` line to commit.

## 5. Permissions

| Action | Permission | Built-in roles |
|---|---|---|
| See the page, discovery, placement, history | `admin:service:read` | SUPER_ADMIN, ADMIN, SUPPORT |
| Start a VM import | `admin:service:manage` | SUPER_ADMIN, ADMIN |
| Save the GitHub integration | `admin:integration:manage` | SUPER_ADMIN, ADMIN |

## 6. Open items

- **Not lab-verified**: no PVE, GitHub token or registry was used while building this. Before enabling:
  run an import against the PVE 9.2 lab (confirm the upload API accepts a ~1–8 GiB `content=import`
  stream with `checksum-algorithm=sha256`, that `import-from` of the result boots, and the exact
  `pveproxy` spool behaviour), with a real classic PAT against the real `proxcloud-vm-*` packages.
- The OCI layer `org.opencontainers.image.title` annotations are what ORAS sets for file pushes; if a
  published artifact lacks them, discovery refuses it (fail closed) — check one real manifest.
- The GitHub packages API returns versions newest first; discovery reads at most 2000 versions per
  package.
- Template-update uploads are serialized per (provider, node, import storage) by the runner's
  PostgreSQL advisory lock. Customer image import's provider-wide upload lock
  (`pve-image-upload-<service>`) is nil-safe and `pve.Service.HolderLock` is **not wired in `cmd/api`
  (pre-existing)**, so a customer import and a template update can still spool to the API node's disk
  at the same time; the room check runs before each. Wiring `HolderLock` fixes that for both.
- **Catalog pruning by `images/build.py`**: a native template imported *and published* here
  (`pc-public;pc-img-<os>;pc-v-<…>` in `pc-templates` on node N) counts as a version for
  `build.py build --keep K` on node N (`versions_of`): the next pipeline build of that OS on that node
  prunes the oldest published versions beyond K, which can include this import (or this import can
  make an older pipeline template the one pruned). `--if-missing` (installer stage 20) also sees it as
  "a version exists" and skips building. A *staged* import (no `pc-public`) and every Kubernetes import
  are not counted and never pruned by `build.py`. Customer VMs are full clones, so pruning a template
  never affects servers.
- **No smoke boot**: unlike `build.py` (which boots a clone and waits for the guest agent before
  publishing), an import is NOT booted before it is tagged. Publishing is therefore opt-in and the
  dialog defaults to *staged*: boot a clone of the staged template, then add `pc-public`.
