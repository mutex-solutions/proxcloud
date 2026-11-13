# Template updates from GitHub Packages

**Admin → System → Template updates** shows the newest VM templates and sandbox images published on
GitHub Packages (`ghcr.io/mutex-solutions`), compares them with what this site runs, and — for VM
templates — imports a chosen artifact as a **new**, staged Proxmox template, smoke-tests it, and lets
the admin publish it (or withdraw a bad version). Scope chosen by the owner (2026-11-12): *the admin
sees the latest available version and clicks update*; lifecycle approved 2026-11-13 (§4). Nothing
updates on its own: a smoke test runs by itself, publishing never does.

| Part | State |
|---|---|
| GitHub integration (PAT, validated on save) | done |
| Discovery: newest VM artifact per line, newest sandbox image per template, installed/pinned state | done |
| VM import job (download by digest → verify → new **staged** template) | done, **off by default** (`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`) until lab-verified |
| Lifecycle: smoke test → Publish → Withdraw + digest blocklist (§4) | done, behind the same flag, **not lab-verified** |
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
 "serviceId":"…","node":"pve1","importStorage":"nas-import","diskStorage":"local-zfs"}
```

Every import lands **staged** (§4). The former `"publish"` field is still parsed for backward
compatibility: absent or `false` is ignored, **`true` is refused with HTTP 400**
(`errors.details.code = "publish_at_import_removed"`) before any provider or GitHub call — a client
that asked for publish-at-import gets a clear error instead of a silently staged template. A digest
on the blocklist (§4) is refused with 409 (`digest_withdrawn`).

**Inputs that the code cannot know are explicit and fail closed**: the provider, node, import
storage and template disk storage are chosen by the admin from
`GET /admin/template-updates/placement/{serviceId}`, which lists per node only the *active* storages
with `import` content and with `images` content (the provider's configured `importStorage` first). A
placement not in that list is refused. The dialog shows the exact OCI digest, disk sha256, sizes and
source commit; `confirmDigest` must repeat the digest.

Enqueue re-lists the package and re-verifies the artifact **server side** (the client's tag/size are
never trusted), then in ONE transaction under an advisory lock on the payload identity: refuses a
duplicate (a receipt for the same provider/node/digest that already produced its template —
`templated`/`succeeded`, whatever its job state — or whose step is not `failed` **and** whose durable
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
  failed build) and tagged **staged**: `proxcloud;pc-src-<12hex>;pc-tu-import;pc-tu-staged`
  (+ `pc-tu-k8s;pc-k8sver-<x-y-z>` for a Kubernetes node image). No job ownership tag and **no catalog
  tag** (`pc-public`, `pc-img-`, `pc-v-`): neither the tenant catalog, `images/build.py` nor an
  installer template selector (stages 45/56 pick by `pc-img-<os>` + `pc-v-`) can pick a staged import.
  The description records the artifact reference, tag and disk sha256. A job that landed its final
  write on the pre-lifecycle version (publish-at-import tags) is accepted as finished when it resumes.
- **Automatic smoke test**: when the job succeeds the runner queues the template's smoke test (§4).
  Without a smoke network on the provider the import records `smoke_network_unset` instead.
- Retries: 3 attempts with backoff; permanent failures (`digest_mismatch`, `pve_checksum_refused`,
  `pve_import_refused`, `pve_forbidden`, `ownership_lost`, `invalid_payload`, …) dead-letter at once.
- **Dead jobs are cleaned up**: a job can die without running its cleanup (e.g. its last attempt's
  lease expired). When idle, the runner re-acquires one dead, uncleaned job with
  `durablejobs.ClaimDead` — a fenced acquisition (new owner, higher epoch) pinned at
  `attempts = max_attempts`, so it can never become a normal run — renews its lease like a run,
  runs the same ownership-checked `Cleanup` (built from the provider alone: cleanup does not need the
  GitHub integration), marks the receipt `cleaned` (`failed`/`job_dead` unless it had finished) and
  returns the job to dead (`cleaned_up`). A terminal failure that completed its own cleanup marks the
  receipt `cleaned` directly.
  - **No hot loop**: a cleanup that cannot run now (provider unreachable) returns the job to dead as
    `cleanup_pending`, and the pass skips it for **10 minutes**; the runner treats that as "no work"
    and sleeps. A provider that is gone for good (`provider_missing`) marks the receipt `cleaned` with a
    warning to remove any leftover by hand, logged once, and is never retried.
- **Replay** through `POST /admin/durable-jobs/{id}/replay` (payload re-validated) requires
  `admin:service:manage` for template-update jobs (the billing key `admin:bill:manage` for billing
  jobs). Only a job that died **mid-flight** (receipt not failed, not finished, not cleaned) can be
  replayed — it resumes from its last receipt. A failed, finished or cleaned template update answers
  **409**: the retry path is a new import from the Template updates page (enqueue allows it, since the
  earlier receipt is failed/dead).
- **Upload serialization**: the runner holds a PostgreSQL session advisory lock **per provider**
  (`templateupdate-upload:<service>`) for the whole upload, across API replicas; a busy lock retries
  later. The key is per provider, not per node/storage: the upload spools to `/var/tmp` of the API node
  pveproxy answers on — one per provider at a time — whatever the target node or import storage.

**Proxmox rights** (the portal token, as for customer image import): `Datastore.AllocateTemplate` on
the import storage (upload), `Sys.Audit` on `/nodes` (upload room check), `VM.Allocate`/`VM.Config.*`
on `/pool/pc-templates` (already granted by `ProxCloudPlatform` on `/pool`) and `Datastore.AllocateSpace`
on the disk storage. Deleting the uploaded file needs `Datastore.Allocate` on the import store; without
it the template is still created and the receipt carries a warning.

The receipt table (`templateUpdate`) is created at API startup: enqueue reads it inside its
transaction, where a missing table cannot be auto-created.

### Enabling

`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true` (or `proxcloud.template-updates.vm-enabled`) starts the
runner (imports **and** smoke tests) and allows enqueueing, smoke, publish and withdraw. Off:
discovery, the template states and the blocklist are still readable; every PVE-mutating action
answers 409 `vm_updates_disabled` and the buttons are disabled. Before turning it on, run one import
+ smoke test per OS on the lab (PVE 9.2): see *Open items*.

## 4. Lifecycle: staged → smoke test → publish → withdraw

Owner decision (2026-11-13): **Staged (hidden) → automatic smoke test → admin Publish (only after a
smoke PASS) → the newest published version is the default for new VMs; older versions are kept;
Withdraw removes a bad version from the catalog (the previous published one takes over) and
blocklists its digest so it can never be published again; running VMs are unaffected. No canary.**

Code: [`lifecycle.go`](../internal/templateupdates/lifecycle.go) (publish, withdraw, blocklist),
[`smoke.go`](../internal/templateupdates/smoke.go) + [`smoke_runner.go`](../internal/templateupdates/smoke_runner.go)
(the smoke job), [`state.go`](../internal/templateupdates/state.go) (states for the page), admin routes in
[`templatelifecycle.go`](../internal/platform/admin/templatelifecycle.go).

| State on the page | Meaning |
|---|---|
| Staged | imported, `pc-tu-staged`, invisible to tenants, no smoke result yet |
| Smoke running | a smoke job is queued or running for exactly this template VMID + digest |
| Smoke failed (code) | the latest smoke of this template ended FAIL; publish refused |
| Ready to publish | the latest smoke of this template passed |
| Published (default) | published, and the newest published version of its line on its node (what new VMs get) |
| Published (older) | published, a newer published version exists (kept; serves as fallback) |
| Withdrawn | digest blocklisted, `pc-tu-withdrawn`, out of every catalog; the template VM is kept |

`GET /admin/template-updates/templates` (`admin:service:read`) lists the newest 200 imports with their
state, latest smoke, withdrawal and allowed actions; `GET /admin/template-updates/blocklist` the
withdrawn digests.

### Smoke test (`templateupdate.smoke/v1`)

`POST /admin/template-updates/templates/{importId}/smoke` (`admin:service:manage`), and automatically
after every successful import. A durable **external** job on the same fenced runner as imports
(lease renewal, `Checkpoint` before every PVE mutation, `Fail` with fixed codes, `ClaimDead` dead-job
pass); a smoke already queued/running for the template is returned instead of a second one. Payload =
exactly one template (service, node, template VMID, digest) plus the smoke network frozen at enqueue.

**Smoke network** (required): `config.templateSmoke` on the Proxmox provider, e.g.
`{"bridge":"vmbr0","vlan":29,"ip":"192.0.2.50/24","gateway":"192.0.2.1","nameserver":"192.0.2.53"}`
(`ip` empty = DHCP; `vlan` 0/absent = untagged). Unset → the smoke cannot run: 409
`smoke_network_unset` ("set config.templateSmoke …"), recorded on the import for the automatic run.
Validated (bridge name, VLAN range, gateway inside the CIDR) at enqueue and in the payload. Smoke tests
are serialized **per provider** (a static smoke IP is one VM at a time): a PostgreSQL session advisory
lock `templateupdate-smoke:<service>`; a busy lock defers the job (`durablejobs.Store.Defer`: back to
pending in 1 minute **without** spending an attempt).

Steps (each PVE mutation right after a successful fence check; the template itself is only read):

1. Re-prove the template: member of `pc-templates` on the node (read from `/pools`), a template, named
   `TemplateName(payload)`, tagged `pc-src-<digest prefix>`; else FAIL `template_mismatch`. A
   withdrawn digest is FAIL `digest_withdrawn`.
2. **Clone** (full, VMID recorded in the receipt *before* the call, lowest free id ≥ 9200) into pool
   **`pc-templates`** on the template's node and disk storage, named `pc-tu-smoke-<16 hex of job id>`.
   Never a tenant pool.
3. **Configure** (one write): tags `proxcloud;pc-tu-smoke;pc-tu-smoke-<job>` (replacing the
   template's copied tags), `net0` on the smoke bridge/VLAN, `ipconfig0`, an **ephemeral ed25519
   SSH key** generated in memory for this attempt (only its SHA-256 fingerprint is recorded; the
   private key is never stored, logged or sent anywhere — nothing ever logs in with it), no password
   (`cipassword` deleted), `agent=1`.
4. **Grow** `scsi0` by +8 GiB, then **start**.
5. Guest agent **ping**, then **`guest-get-osinfo`** within 5 minutes; the reported `ID`/`VERSION_ID`
   must match the artifact (`debian-13` → `debian`/`13`, `ubuntu-2604` → `ubuntu`/`26.04`).
6. **cloud-init done**: `guest-exec` of `/usr/bin/cloud-init status --format json` (output bounded to
   64 KiB, plain `cloud-init status` as fallback for older cloud-init), polled for up to 10 minutes:
   `done` passes (`degraded done` passes with a warning), `error`/`disabled` fail.
7. **Root disk grew**: the `/` filesystem size from `guest-get-fsinfo` (fallback `df -B1` through the
   agent) must exceed the template's whole disk size — only cloud-init's growpart can make it so.
8. Kubernetes node image only: `containerd` and `kubelet` present and `kubelet --version` =
   `Kubernetes v<artifact version>` (no join, no kubeadm).
9. Record **PASS**/**FAIL** with a fixed reason code, `startedAt`/`finishedAt`, the observed values
   (OS name, cloud-init status, root fs bytes, kubelet version) — then **stop and destroy** the test
   VM, and only then mark the receipt `finished` (`cleanedAt`).

FAIL codes: `template_mismatch`, `digest_withdrawn`, `clone_failed`, `configure_failed`,
`resize_failed`, `start_failed`, `agent_timeout`, `osinfo_failed`, `osinfo_mismatch`,
`cloudinit_timeout`, `cloudinit_failed`, `cloudinit_unreadable`, `disk_not_grown`,
`disk_unverifiable`, `k8s_components_missing`, `k8s_version_mismatch`; terminal infrastructure codes
`guest_exec_forbidden`, `pve_forbidden`, `ownership_lost`, `attempts_exhausted`, `executor_unavailable`,
`provider_missing`, `invalid_payload`, `job_dead`. A check FAIL is a *finished* smoke (the job
succeeds; the result is FAIL); transport errors are retried (3 attempts) and then recorded as FAIL.

**Ownership and cleanup.** The only VM a smoke job mutates or destroys is its own test VM: a member
of `pc-templates` on the job's node, **not a template**, carrying the job's owner tag — or, in the
window between the clone and its config write (when it still carries the template's copied tags),
the job's exact name. Every stop/destroy re-reads that proof first; a VM outside `pc-templates` (a
tenant or CAPMOX copy) is never considered. Cleanup is always attempted: right after the result is
recorded, at the end of an attempt that hit a retryable error, by a new acquisition (which never
trusts an earlier attempt's test VM: the key died with that process), after a terminal failure, and by
the runner's dead-job pass (`DeadUncleaned` on `templateUpdateSmoke`, fenced `ClaimDead`, receipt
gets FAIL `job_dead`, 10-minute throttle when the provider is unreachable). Smoke jobs are never
replayed (409): start a new one. Bounds: clone/config/power task 30 minutes, boot 5 minutes, cloud-init
10 minutes, one guest command 60 seconds, a whole acquisition 1 hour. Nothing runs on the API host.

### Publish

`POST /admin/template-updates/templates/{importId}/publish` `{"confirmDigest":"sha256:…"}`
(`admin:service:manage`, audited). Under a per-digest advisory lock: refused unless the **latest
smoke of exactly that template VMID + digest** on that provider node is **PASS** (`smoke_missing`,
`smoke_running`, `smoke_not_passed`; a PASS of another copy of the same digest does not count) and the
digest is not blocklisted (`digest_withdrawn`). It re-proves the template (as in smoke step 1) and
writes, with the config digest just read (a concurrent config change makes PVE refuse instead of
overwriting), then reads the tags back:

- native OS: `proxcloud;pc-public;pc-img-<os>;pc-v-<yyyymmddhhmm>` (leading, as the pipeline writes
  them, so installer selectors that sort by the tag string order versions correctly) +
  `pc-src-…;pc-tu-import`; `pc-tu-staged` removed. `pve.DiscoverTemplates` then serves the newest
  `pc-v-` per node — the new version becomes the default for new VMs, older ones stay.
- Kubernetes node image: `pc-tu-k8s-public` + `pc-v-<…>` — **never** `pc-public`/`pc-img-` (a node
  image is not a tenant OS catalog entry; lead decision 2026-11-13). The page shows the exact
  `config.cluster.versions` line (`"1.35.10": "pve1=9210,pve2=9211"`, newest published per node) to
  put on the kamaji provider; the portal does not edit that map (the provider update path replaces
  `config` keys wholesale, so a single-key edit could not be made non-destructive).

Idempotent: an already published template answers `unchanged: true` and writes nothing.

### Withdraw

`POST /admin/template-updates/templates/{importId}/withdraw`
`{"reason":"…","confirmDigest":"sha256:…","force":false,"confirmLast":""}` (`admin:service:manage`,
audited with the reason). A reason of 8–500 characters is required. Then, under the per-digest lock:

1. **Last-version guard**: if a copy of this artifact is published and no *other* published template
   serves the line on the provider (any published `pc-img-<os>` for an OS; any published import or
   pipeline `pc-k8s-<ver>` template for a Kubernetes version), it is refused with 409
   `last_published_version` unless `force: true` **and** `confirmLast` = the line's slug
   (`debian-13`, `ubuntu-2604-k8s-1.35.10`). The dialog shows the consequence: no new VM of that OS
   can be created until another version is published.
2. The digest goes on the **blocklist** (`templateUpdateBlocklist`, id = digest: package, tag,
   reason, actor, time, provider, node, VMID, import id). The first withdrawal is kept; repeating it
   changes nothing.
3. **Every copy** of the artifact on the provider (finished imports of that digest, any node) gets
   `pc-tu-withdrawn` (+ `pc-tu-import`), and loses `pc-public`, `pc-tu-k8s-public`, `pc-tu-staged` and
   `pc-img-<os>` (so no installer selector picks it either). The template VMs are **kept** for
   forensics, never deleted.

`pve.DiscoverTemplates` skips any template carrying `pc-tu-withdrawn`, even if someone puts
`pc-public` back by hand, and falls back to the previous published version (newest `pc-v-` wins).
If a copy's tag change does not land, the digest is still blocklisted and the answer says
`complete: false` with the copies in `warnings`; repeating the withdraw finishes it. Running VMs are
full clones and are never touched.

**Blocklisted digests** are shown as *withdrawn* in discovery (the newest artifact row) and refused
by import (409), smoke (409, and FAIL `digest_withdrawn` if already queued) and publish (409).

### Audit

Smoke, publish and withdraw are audited by the admin audit middleware (resource type
`TEMPLATE_UPDATES`, the actor from the token) with a snapshot: action, import id, digest, line slug,
provider, changed VMIDs, the smoke job publish relied on, and the withdraw reason / force / remaining
published VMIDs. Refused requests are not audited as successes.

### Interaction with `images/build.py` pruning

`build.py build --keep K` prunes the oldest published versions of an OS on a node. It now never
prunes (a) a template carrying the import marker `pc-tu-import` — the portal owns its lifecycle — and
(b) the newest published version (`--keep` below 1 is treated as 1, so the only published version always
stays). Imports still count as versions for `--if-missing` (stage 20 skips building when an imported
version is published there) and take no `--keep` slot (pipeline versions keep K slots of their own).
Staged and withdrawn imports carry no `pc-public`/`pc-img-` at all, so `build.py` never even lists them.
A pipeline build publishes `pc-public` as before, so it can become the default over an older import
(newest `pc-v-` wins) — expected.

## 5. Sandbox templates — not yet enabled (follow-up design)

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

## 6. Permissions

| Action | Permission | Built-in roles |
|---|---|---|
| See the page, discovery, placement, history, template states, blocklist | `admin:service:read` | SUPER_ADMIN, ADMIN, SUPPORT |
| Start a VM import, run a smoke test, publish, withdraw | `admin:service:manage` | SUPER_ADMIN, ADMIN |
| Save the GitHub integration | `admin:integration:manage` | SUPER_ADMIN, ADMIN |

Proxmox rights of the portal token for the smoke test, on `/pool/pc-templates` (already in
`ProxCloudPlatform` on `/pool`): `VM.Clone`, `VM.Allocate`, `VM.Config.Network`/`Cloudinit`/`Disk`/
`Options`, `VM.PowerMgmt`, `VM.GuestAgent.Audit` (ping, osinfo, fsinfo) and
**`VM.GuestAgent.Unrestricted`** (guest-exec: `cloud-init status`, `df`, `kubelet --version`; without it
the smoke ends FAIL `guest_exec_forbidden`); `Datastore.AllocateSpace` on the template disk storage;
`SDN.Use` on the smoke bridge's zone (`/sdn/zones/localnetwork/<bridge>` for a plain bridge on PVE 8+).
Publish/withdraw need `VM.Config.Options` (tags) only.

## 7. Open items

- **Not lab-verified**: no PVE, GitHub token or registry was used while building this. Before enabling:
  run an import against the PVE 9.2 lab (confirm the upload API accepts a ~1–8 GiB `content=import`
  stream with `checksum-algorithm=sha256`, that `import-from` of the result boots, and the exact
  `pveproxy` spool behaviour), with a real classic PAT against the real `proxcloud-vm-*` packages.
- The OCI layer `org.opencontainers.image.title` annotations are what ORAS sets for file pushes; if a
  published artifact lacks them, discovery refuses it (fail closed) — check one real manifest.
- The GitHub packages API returns versions newest first; discovery reads at most 2000 versions per
  package.
- Template-update uploads are serialized per provider by the runner's PostgreSQL advisory lock. Customer image import's provider-wide upload lock
  (`pve-image-upload-<service>`) is nil-safe and `pve.Service.HolderLock` is **not wired in `cmd/api`
  (pre-existing)**, so a customer import and a template update can still spool to the API node's disk
  at the same time; the room check runs before each. Wiring `HolderLock` fixes that for both.
- **Catalog pruning by `images/build.py`**: see §4 — imports (`pc-tu-import`) are never pruned and
  the only published version is never pruned; imports count for `--if-missing`. A template imported
  *and published* by the pre-lifecycle version carries no import marker: `build.py` may prune it like
  a pipeline template until it is withdrawn (which adds the marker) or published again through the
  lifecycle (Run smoke works on such an import — its name and `pc-src-` tag prove it — and Publish then
  adds the marker).
- **Last-version guard scope**: the guard counts published versions of the line on the whole provider,
  not per node. On a site with local (non-shared) template storage, withdrawing a node's only copy
  while another node still has one is allowed; new VMs scheduled to that node then cannot be cloned
  there until a version is published on it.
- **Smoke test — needs lab verification (PVE 9.2), nothing of it ran against a real cluster**:
  - the guest agent endpoints: `agent/get-osinfo` / `agent/get-fsinfo` answer shapes
    (`version-id`, `total-bytes` present on the shipped qemu-guest-agent), `agent/exec` + `exec-status`
    with `cloud-init status --format json` on each OS (Debian 12 ships cloud-init 22.4 — the plain
    `status:` fallback covers it; check the real output);
  - that the artifact's templates have qemu-guest-agent enabled at boot and growpart in cloud-init,
    so a +8 GiB grow shows up as a larger `/` (an image whose root is not the last partition, or LVM,
    would FAIL `disk_not_grown`);
  - the timeouts (boot 5 min, cloud-init 10 min) on nested/slow nodes;
  - `sshkeys` encoding and that `ipconfig0` static/DHCP on the smoke bridge+VLAN work; SDN.Use for
    the bridge; the full clone of a large template within the 30-minute task bound;
  - the Kubernetes node image: `kubelet --version` output exactly `Kubernetes v<x.y.z>` and
    `containerd` on `PATH` of the image-builder image.
- Concurrent pipeline builds: `build.py` names its test VM `smoke-…` and never touches
  `pc-tu-smoke-…` VMs; a smoke test never touches pipeline VMs. Both use the lowest free VMID ≥ 9200 —
  a race is retried on "already exists".
