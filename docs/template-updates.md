# Template updates from GitHub Packages

**Admin → System → Template updates** shows the newest VM templates and sandbox images published on
GitHub Packages (`ghcr.io/mutex-solutions`), compares them with what this site runs, and — for VM
templates — imports a chosen artifact as a **new**, staged Proxmox template, smoke-tests it, and lets
the admin publish it (or withdraw a bad version); for sandbox images — mirrors the chosen digest into
the sandbox registry and pre-pulls it before templates switch. Scope chosen by the owner (2026-11-12):
*the admin sees the latest available version and clicks update*; the lifecycle (§4) and the sandbox
mirror (§5) were approved 2026-11-13. Nothing updates on its own: a smoke test runs by itself,
publishing never does.

| Part | State |
|---|---|
| GitHub integration (PAT, validated on save) | done |
| Discovery: newest VM artifact per line, newest sandbox image per template, installed/pinned state | done |
| VM import job (download by digest → verify → new **staged** template) | done, **off by default** (`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`) until lab-verified |
| Lifecycle: smoke test → Publish → Withdraw + digest blocklist (§4) | done, behind the same flag, **not lab-verified** |
| Sandbox mirror → prepull → pin switch from the portal (+ rollback) | done, **off by default** (`PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true`) until cluster-verified (§5) |

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
- **Discovery uses it**: REST calls send `Authorization: Bearer <PAT>`; the VM image registry calls
  exchange it (HTTP Basic, only to the realm on the registry's own host) for a pull-scoped registry
  token. Without the integration the page shows *Configure the GitHub integration* and the API answers
  409. The one exception is the **sandbox mirror's worker token** (§5.2): the sandbox packages are
  public, so the worker gets an anonymous pull token, checked never to be or contain the PAT; the PAT
  never reaches the sandbox cluster.
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
    duplicate keys refused, integer-only sizes, disk < 8 GiB, virtual size 5–100 GiB (20–100 GiB up to v0.5.2), receipt checks
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

A receipt whose template was deleted since stops blocking only through `Service.MarkTemplateGone`
(no route yet; the managed-services job calls it). It reads the receipt's own provider and marks the
receipt `templateGone` only when no guest in the cluster holds the receipt's VMID any more, or the VM
that does is visible to the portal token, has a name, sits on a node that is up and carries neither
the template's name nor its `pc-src-` tag (the VMID was reused), or the VM there is another
import's template of the same artifact: a finished, unmarked receipt of the same provider records
exactly that node and VMID and was created after this receipt's last write. Every import takes the
lowest free VMID from 9200, so once a template is deleted, a copy of its artifact imported on another
node can take its VMID; without this rule that copy would block the node of the deleted one. "No
guest" comes from `GET /cluster/nextid?vmid=`, which reads the cluster's whole guest list;
`/cluster/resources` lists only the guests the token has `VM.Audit` on (through its pools), so a VMID
missing from it proves nothing. A read error, a node that is down, a blank row, a held VMID the token cannot see (a template
moved out of `pc-templates`, or a token that lost its pool rights), or a template that was only
renamed or migrated changes nothing. Such a template keeps blocking a re-import on the receipt's node
until the operator deletes it in Proxmox (the next call then frees the receipt) or puts it back (its
node, name and pool). A marked receipt then counts as failed for the duplicate check and shows
`template_missing` on the page.

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
- **Pool**: before the upload the job reads pool `pc-templates` and creates it when the cluster has
  none (a new install that skipped `pveum pool add pc-templates` of `docs/proxmox-permissions.md`). An
  existing pool is only read. A token that may not create it fails the job as `pve_forbidden` before
  any byte is downloaded.
- **Resume**: a new acquisition continues from the last committed step. `uploaded` is reused while
  the verified file is still in the import store; an intent-only `uploading` (outcome unknown) is
  redone under a fresh, epoch-specific file name, so a superseded runner still uploading can never
  collide; `creating` with a VMID re-reads that VM: ours and imported → continue, ours and empty →
  delete and recreate, someone else's → never touched, pick another VMID.
- **Ownership**: the job creates its VM named `pc-tu-build-<16 hex of job id>` **without tags**, then
  tags it `pc-tu-<16 hex of job id>` with the import write, and names its files
  `pc-tu-<…>-e<epoch>-<digest prefix>.qcow2`. A create carries no tags because PVE 9 checks a create's
  tags with `VM.Config.Options` on `/vms/<id>` without the pool, and the token's rights reach a VM only
  through its pool (no ACL on `/vms`): verified on PVE 9.2.21, where a tagged create fails 403. A VM is
  the job's to mutate or destroy only if **all** hold: it is a member of pool `pc-templates` on the
  job's node (read from `/pools/pc-templates`), it carries that tag (or, in the window between the
  create and the import write, has no tag at all and exactly that build name), and it is not
  `pc-public`. The orphan sweep only
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
  (`images/build.py`, `pve.ImportImage`; no NIC, cloud-init drive on the disk storage: on `ide2` for an
  OS image, on `ide0` for a Kubernetes node image, see below; a Kubernetes node image also gets
  `onboot: 1`, which every clone copies, so workers and platform machines made from it start again
  with their Proxmox node; a node image imported before v0.9.0 gets it at the next provider sync:
  [managed-services.md](managed-services.md#after-a-proxmox-node-reboots); display `vga: std` with
  `serial0: socket` from v0.10.0, see [Console display](#console-display)), named
  `pc-ghcr-<os>-<os|k8s-x-y-z>-<build>` (never `pc-<os>-…`, which `build.py` would treat as its own
  failed build) and tagged **staged**: `proxcloud;pc-src-<12hex>;pc-tu-import;pc-tu-staged`
  (+ `pc-tu-k8s;pc-k8sver-<x-y-z>` for a Kubernetes node image). No job ownership tag and **no catalog
  tag** (`pc-public`, `pc-img-`, `pc-v-`): neither the tenant catalog, `images/build.py` nor an
  installer template selector (stages 45/56 pick by `pc-img-<os>` + `pc-v-`) can pick a staged import.
  The description records the artifact reference, tag and disk sha256. A job that landed its final
  write on the pre-lifecycle version (publish-at-import tags) is accepted as finished when it resumes.
- **Automatic smoke test**: when the job succeeds the runner queues the template's smoke test (§4).
  Without a smoke network on the provider the import records `smoke_network_unset` instead. An import
  a managed-services platform run made (requested by `platform-run:<run id>`) gets none: each test is a
  full clone on the disks the run is building its seed and cluster on. The import records
  `smoke_not_started`, and the test is started by hand from Template updates before a Publish.
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
  earlier receipt is failed/dead). A smoke test is never replayed (409, §4); a sandbox mirror or
  rollback is judged by its own receipt the same way (§5).
- **Upload serialization**: the runner holds a PostgreSQL session advisory lock **per provider**
  (`templateupdate-upload:<service>`) for the whole upload, across API replicas; a busy lock retries
  later. The key is per provider, not per node/storage: the upload spools to `/var/tmp` of the API node
  pveproxy answers on — one per provider at a time — whatever the target node or import storage.

**Proxmox rights** (the portal token, as for customer image import): `Datastore.AllocateTemplate` on
the import storage (upload), `Sys.Audit` on `/nodes` (upload room check), `VM.Allocate`/`VM.Config.*`
on `/pool/pc-templates` (already granted by `ProxCloudPlatform` on `/pool`, as is `Pool.Allocate`, which creates
`pc-templates` when it is missing) and `Datastore.AllocateSpace`
on the disk storage. Deleting the uploaded file needs `Datastore.Allocate` on the import store; without
it the template is still created and the receipt carries a warning.

The receipt table (`templateUpdate`) is created at API startup, with the smoke/blocklist tables (§4)
and the sandbox receipt table (§5): the enqueues read them inside their transaction, where a missing
table cannot be auto-created.

### Enabling

`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true` (or `proxcloud.template-updates.vm-enabled`) starts the
runner (imports **and** smoke tests) and allows enqueueing, smoke, publish and withdraw. Off:
discovery, the template states and the blocklist are still readable; every PVE-mutating action
answers 409 `vm_updates_disabled` and the buttons are disabled. Before turning it on, run one import
+ smoke test per OS on the lab (PVE 9.2): see *Open items*.

On the Docker Compose install the variable is set by `./proxcloud-up.sh install --template-updates`
(`--no-template-updates` clears it): [deploy/compose/README.md](../deploy/compose/README.md#optional-features).

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
| Withdrawn | digest blocklisted **and this copy** tagged `pc-tu-withdrawn`: out of every catalog; the template VM is kept |
| Withdraw incomplete | digest blocklisted but **this copy** is not tagged withdrawn yet (its provider was unreachable or the tag write failed); Withdraw stays enabled and finishes it |

`GET /admin/template-updates/templates` (`admin:service:read`) lists the newest 200 imports with their
state, latest smoke, withdrawal and allowed actions; `GET /admin/template-updates/blocklist` the
withdrawn digests.

### Smoke test (`templateupdate.smoke/v1`)

`POST /admin/template-updates/templates/{importId}/smoke` (`admin:service:manage`), and automatically
after every successful import except those of a platform run (above). A durable **external** job on the same fenced runner as imports
(lease renewal, `Checkpoint` before every PVE mutation, `Fail` with fixed codes, `ClaimDead` dead-job
pass); a smoke already queued/running for the template is returned instead of a second one. Payload =
exactly one template (service, node, template VMID, digest) plus the smoke network frozen at enqueue.

**Smoke network** (required): `config.templateSmoke` on the Proxmox provider, e.g.
`{"bridge":"vmbr0","vlan":29,"ip":"192.0.2.50/24","gateway":"192.0.2.1","nameserver":"192.0.2.53"}`
(`ip` empty = DHCP; `vlan` 0/absent = untagged). Unset → the smoke cannot run: 409
`smoke_network_unset` ("set config.templateSmoke …"), recorded on the import for the automatic run.
Validated at enqueue and in the payload: bridge/vnet name, VLAN 0–4094, **IPv4 only** for a static
address (a host address in a /8–/30, gateway another address inside it; an IPv6-only smoke network is
not supported), nameserver a plain address without a zone. At enqueue an **untagged** smoke network
on a bridge that carries the node's management address or default gateway is refused (409
`smoke_network_management_bridge`, the same check as public networks): the test VM would sit on the
hypervisor's L2. A VLAN-tagged network on such a bridge is allowed (its own segment, as the image
pipeline builds). Smoke tests
are serialized **per provider** (a static smoke IP is one VM at a time): a PostgreSQL session advisory
lock `templateupdate-smoke:<service>`; a busy lock defers the job (`durablejobs.Store.Defer`: back to
pending in 1 minute **without** spending an attempt).

Steps (each PVE mutation right after a successful fence check; the template itself is only read):

1. Re-prove the template: member of `pc-templates` on the node (read from `/pools`), a template, named
   `TemplateName(payload)`, tagged `pc-src-<digest prefix>`; else FAIL `template_mismatch`. A
   Kubernetes node template with a Proxmox cloud-init drive on another slot than `ide0` is FAIL
   `template_mismatch` too, with the command that removes the drive (below); nothing is cloned. A
   withdrawn digest is FAIL `digest_withdrawn`. The template's **config digest** is recorded in the
   receipt with the clone's VMID: a PASS vouches for exactly that configuration.
2. **Clone** (full, VMID recorded in the receipt *before* the call, lowest free id ≥ 9200) into pool
   **`pc-templates`** on the template's node and disk storage, named `pc-tu-smoke-<16 hex of job id>`.
   Never a tenant pool.
3. **Configure** (one write): tags `proxcloud;pc-tu-smoke;pc-tu-smoke-<job>` (replacing the
   template's copied tags), `net0` on the smoke bridge/VLAN, `ipconfig0`, an **ephemeral ed25519
   SSH key** generated in memory for this attempt (only its SHA-256 fingerprint is recorded; the
   private key is never stored, logged or sent anywhere — nothing ever logs in with it), no password
   (`cipassword` deleted) and no custom cloud-init snippets (`cicustom` deleted), `agent=1`, and
   `ciupgrade=0`: the test VM does not upgrade its packages at first boot (Proxmox's default is on), so
   the smoke network needs no route to a package mirror and the verdict is about the image as
   published. The option exists since Proxmox VE 8.0, below the supported 9.2; a Proxmox that refuses
   it gets the write without it and the record a warning. When the
   clone has **no Proxmox cloud-init drive**, the same write adds one on the template's disk storage,
   in the slot an import of that variant uses (`ide0` for a Kubernetes node image, `ide2` for an OS
   image): these settings reach the guest only through that drive, and without it cloud-init ends
   `disabled`. A node template without the drive is a valid one (below, and stage 7 of managed
   services tells an operator to remove a drive that is not on `ide0`). A clone that has a drive
   (`ide0` on a node image, any slot on an OS image) keeps it and gets no second one. The template itself is never changed.
4. **Grow** `scsi0` by +8 GiB, then **start**.
5. Guest agent **ping**, then **`guest-get-osinfo`** within 5 minutes; the reported `ID`/`VERSION_ID`
   must match the artifact (`debian-13` → `debian`/`13`, `ubuntu-2604` → `ubuntu`/`26.04`).
6. **cloud-init done**: `guest-exec` of `/usr/bin/cloud-init status --format json` (output bounded to
   64 KiB, plain `cloud-init status` as fallback for older cloud-init), polled for up to 10 minutes:
   `done` passes (`degraded done` passes with a warning), `error`/`disabled` fail. On a failure the
   errors of `/run/cloud-init/result.json` are read through the agent before the test VM is destroyed
   and added to the record's message (printable characters only, at most 300).
7. **Root disk grew**: the `/` filesystem size from `guest-get-fsinfo` (fallback `df -B1` through the
   agent) must exceed the template's whole disk size — only cloud-init's growpart can make it so.
8. Kubernetes node image only: `containerd` and `kubelet` present and `kubelet --version` =
   `Kubernetes v<artifact version>` (no join, no kubeadm).
9. Record **PASS**/**FAIL** with a fixed reason code, `startedAt`/`finishedAt`, the observed values
   (OS name, cloud-init status, root fs bytes, kubelet version) — then **stop and destroy** the test
   VM, and only then mark the receipt `finished` (`cleanedAt`). A destroy task that ends with
   warnings has removed the VM and may have kept a disk (ZFS: `dataset is busy`): the volumes the test
   VM's config named are looked up on their store, freed again up to three times (at once, then twice
   one minute apart), and what still stays is a warning on the record that names each volume and the
   command (`pvesm free <volume>`). Only those exact volumes are freed, never another volume that
   carries the same VMID, and none once a VM holds that VMID again. The names are saved on the record
   before the destroy, so an attempt that finds the VM already gone (the destroy outlasted its wait,
   or the API restarted) checks them too. Listing needs `Datastore.Audit` and freeing
   `Datastore.Allocate` on the store. **The portal token as documented above holds only
   `Datastore.AllocateSpace` there, so on such an install nothing is freed**: the first free is
   refused, there is no retry, and the warning says so; remove the volume by hand, or grant the
   token `Datastore.Allocate` on the template disk storage (in Proxmox that right opens every volume
   on the store). Without `Datastore.Audit` nothing is checked unless the task itself warned.

FAIL codes: `template_mismatch`, `digest_withdrawn`, `clone_failed`, `configure_failed`,
`resize_failed`, `start_failed`, `agent_timeout`, `osinfo_failed`, `osinfo_mismatch`,
`cloudinit_timeout`, `cloudinit_failed`, `cloudinit_unreadable`, `disk_not_grown`,
`disk_unverifiable`, `k8s_components_missing`, `k8s_version_mismatch`; terminal infrastructure codes
`guest_exec_forbidden`, `pve_forbidden`, `ownership_lost`, `attempts_exhausted`, `executor_unavailable`,
`provider_missing`, `invalid_payload`, `job_dead`. A check FAIL is a *finished* smoke (the job
succeeds; the result is FAIL); transport errors are retried (3 attempts) and then recorded as FAIL.

**Ownership and cleanup.** The only VM a smoke job mutates or destroys is its own test VM: a member
of `pc-templates` on the job's node, **not a template**, named **exactly** `pc-tu-smoke-<job>`, and
carrying the job's owner tag — or, only in the window between the clone and its config write, no
`pc-tu-smoke*` tag at all (it still carries the template's copied tags). A manual clone of a test VM
(another name, copied owner tag) or a VM configured by another smoke job is never touched. A test VM
that cannot be removed keeps the smoke receipt *uncleaned* (warning recorded): the attempt is retried,
then the dead-job pass retries every 10 minutes until it is gone. Every stop/destroy re-reads that proof first; a VM outside `pc-templates` (a
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
`smoke_running`, `smoke_not_passed`; a PASS of another copy of the same digest does not count), the
digest is not blocklisted (`digest_withdrawn`), and the template's **config digest is still the one the
smoke test recorded** — any change of the template's configuration after the smoke (disk, cloud-init,
hardware, tags) refuses with 409 `template_changed_since_smoke`: run the smoke again. It re-proves the template (as in smoke step 1) and
writes, with the config digest just read (a concurrent config change makes PVE refuse instead of
overwriting), then reads the tags back:

- native OS: `proxcloud;pc-public;pc-img-<os>;pc-v-<yyyymmddhhmm>` (leading, as the pipeline writes
  them, so installer selectors that sort by the tag string order versions correctly) +
  `pc-src-…;pc-tu-import`; `pc-tu-staged` removed. `pve.DiscoverTemplates` then serves the newest
  `pc-v-` per node — the new version becomes the default for new VMs, older ones stay.
- Kubernetes node image: `pc-tu-k8s-public` + `pc-v-<…>` — **never** `pc-public`/`pc-img-` (a node
  image is not a tenant OS catalog entry; lead decision 2026-11-13); any `pc-public`/`pc-img-*` put on
  it by hand is removed by the publish. The page shows the exact
  `config.cluster.versions` line (`"1.35.10": "pve1=9210,pve2=9211"`, newest published per node) to
  put on the kamaji provider; the portal does not edit that map (the provider update path replaces
  `config` keys wholesale, so a single-key edit could not be made non-destructive).

Idempotent: an already published template answers `unchanged: true` and writes nothing.

### Publish on all nodes

A site without shared storage needs its own copy of a Kubernetes node image on every node: workers are cloned only on
the node that holds the template ([managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox)). **Publish on all
nodes…** on a row of the **VM templates** tab brings one artifact digest to published on every node of one Proxmox
provider. It works for any VM template; the versions line and the missing-node warning are for Kubernetes node images.

- `GET /admin/template-updates/publish-all?serviceId=…&digest=sha256:…` (`admin:service:read`) is the report and
  changes nothing.
- `POST /admin/template-updates/publish-all` `{"serviceId","package","digest","confirmDigest"}`
  (`admin:service:manage`, audited with what it did) is **one advance** and answers the same report. Two advances of
  one provider never run at once (a PostgreSQL advisory lock; the second answers 409 `busy`).

One advance, in this order:

1. **Publish** every node whose latest smoke test passed, through the Publish above with all its gates. A refusal on
   one node is reported for that node (`publish_failed` with the code) and the other nodes go on.
2. **Start at most one job**, and only when no import and no smoke test of that provider is queued or running, of this
   artifact or any other (several clones at once overload a small cluster):
   - a smoke test of the first imported node that has none, on the provider's smoke network. A network that is unset
     or refused for that node is recorded on the import, as the automatic smoke test does, and reported as
     `smoke_not_started`; the next candidate is tried;
   - else an import on the first node without a copy. It goes to the provider's `importStorage` and to the default
     storage profile's storage for that node, and both must be active on the node (the rule of the import dialog).
     When the import finishes the automatic smoke test starts by itself.

Nothing runs in the background. The dialog reads the report every 15 s and advances again while `next` is `advance`;
closing it pauses, and opening it again continues from the state on the provider. When a publish was refused, or an
advance answered an error, the dialog stops and shows the reason; **Try again** repeats it.

An advance that fails before it changed anything answers the error (for example 429 while GitHub rate-limits the
import). One that already published a node in step 1 and then cannot start its job in step 2 answers 200 with the
report and `error: {code, message}`: the publish is in `actions` and in the audit log, no job is started, and `next`
is `advance`.

| Node state | Meaning |
|---|---|
| `published` | The copy on this node is published |
| `ready_to_publish` | Its smoke test passed; the next advance publishes it |
| `publish_failed` | Publish refused (`code`: `template_changed_since_smoke`, `pve_tag_write_failed`, …) |
| `smoke_running`, `smoke_queued` | A smoke test of the copy is queued or running (`smoke_queued`: this advance queued it) |
| `staged` | Imported; waits its turn for a smoke test |
| `smoke_not_started` | The smoke test could not be queued (`code`, e.g. `smoke_network_unset`). Fix the cause and run it from **Imported templates** |
| `smoke_failed` | The smoke test failed (`code` as stored). Not repeated, however old the failure is and whatever caused it: run the copy's smoke test again from its row under **Imported templates** (`POST /admin/template-updates/templates/{importId}/smoke`). When it passes the node reads `ready_to_publish` and the next advance publishes it. The node's `message` ends with this hint |
| `importing`, `import_queued` | An import is queued or running (`import_queued`: this advance queued it) |
| `import_failed` | The import failed (`code` as stored). Not repeated: start it again with **Import…** |
| `missing` | No copy; waits its turn for an import |
| `present_unmanaged` | A template with this artifact's source tag that the portal did not import is on the node. Nothing is imported over it |
| `skipped` | The node is not online (`code` `node_down`, also when it holds a copy that cannot be read while it is down), its storages are not active, the provider has no `importStorage`, the default profile names no storage for it, an earlier import blocks a new one, or the copy is tagged withdrawn (`message` says which) |

The report also carries:

- `versionsLine`: the `config.cluster.versions` entry of the published copies, newest per node (as after a Publish).
  It is still display only: the operator copies it into the Kubernetes provider.
- `nodesMissing`: nodes that publish a node image of another Kubernetes version and none of this one.
- `complete`: every node is `published` or `skipped` for a reason other than being down, and `nodesMissing` is empty.
  A node that is down is left out of `versionsLine` even when it holds a published copy, so the report is not
  complete until it is back (or removed from the cluster). While it is false the dialog warns
  before the line: a line that covers fewer nodes moves a cluster's workers off the missing nodes at its next node
  group change or upgrade, and ArgoCD deletes the workers of a dropped node at once
  ([managed-k8s.md](managed-k8s.md#9-how-a-cluster-lands-on-proxmox)).
- `next`: `advance`, `wait` (a job of the provider is running), `blocked` or `done`. `blocked`: nothing runs and a
  node is `smoke_failed`, `smoke_not_started`, `import_failed` or `publish_failed`. Each such node's `message` says
  what to do. An advance never repeats a smoke test or an import, so for the first three posting again changes
  nothing; `publish_failed` is the refusal of the publish that post just tried, and the next post tries it again
  (the dialog does not post by itself while `blocked`). The dialog says
  "Blocked" instead of "Nothing more to do". It keeps reading every 15 s and goes on by itself once the copy's own
  smoke test has passed. Before, such a report read `done` with `complete: false` (seen on a lab: two copies with an
  eight-hour-old failed smoke test stopped the action, and each had to be smoke-tested from its row). There is no
  automatic retry. `done` with `complete: false` is left for what no step of the operator in this dialog changes: a
  node that is down, a skipped node, a copy the portal did not import, a node that publishes another version.
- `actions`: what this call did (`published pve2/9211`, `smoke test queued pve3/9212`, `import queued pve4`).
- `error`: only on an advance that acted and then could not go on (above).

It never withdraws or removes tags, never imports where a copy of the digest exists, never repeats a failed import or
smoke test, never bypasses a publish gate, and never starts a second job of a provider. A node whose import or smoke
test failed keeps whatever older copy it has published, and stays in the line with it. A withdrawn digest is refused
(409 `digest_withdrawn`), and so is the whole action while `PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED` is off.

### Withdraw

`POST /admin/template-updates/templates/{importId}/withdraw`
`{"reason":"…","confirmDigest":"sha256:…","force":false,"confirmLast":"","confirmNodeLoss":false}`
(`admin:service:manage`, audited with the reason and the per-provider outcome). A reason of 8–500
characters is required. A withdraw is about the **artifact digest**, so it covers **every Proxmox
provider** that holds a finished import of it (any import receipt of that digest, whichever row the
action was started from). Under the per-digest lock:

1. Every such provider's cluster is read and planned (copies of the artifact, other published versions
   of the line). **Guards, all evaluated before anything changes:**
   - **last version per provider**: a provider left without any published version of the line (any
     published `pc-img-<os>` for an OS; any published import or pipeline `pc-k8s-<ver>` template for a
     Kubernetes version) → 409 `last_published_version` naming the providers, unless `force: true`
     **and** `confirmLast` = the line's slug (`debian-13`, `ubuntu-2604-k8s-1.35.10`);
   - **nodes losing their last copy**: a node left without a published version of the line while the
     provider keeps one elsewhere (node-local template storage: VMs there clone only from that node's
     copy) → 409 `nodes_losing_last_copy` listing `<provider>/<node>`, unless `confirmNodeLoss: true`.
   The page shows both consequences in the dialog and requires the confirmations.
2. The digest goes on the **blocklist** (`templateUpdateBlocklist`, id = digest: package, tag,
   reason, actor, time, provider, node, VMID, import id). The first withdrawal is kept; repeating it
   changes nothing there.
3. On each provider, **every copy** of the artifact (finished imports of that digest, any node) gets
   `pc-tu-withdrawn` (+ `pc-tu-import`), and loses `pc-public`, `pc-tu-k8s-public`, `pc-tu-staged` and
   `pc-img-<os>` (so no installer selector picks it either). The template VMs are **kept** for
   forensics, never deleted.
4. The result lists each provider: `withdrawn` (every copy there tagged), `incomplete` (a tag write
   failed, or a copy's node is down), `unreachable` (the provider or its cluster could not be read —
   **nothing** was changed there) or `provider_missing` (the provider was deleted). `complete` is false
   if any provider is `incomplete`/`unreachable`.

**Withdraw incomplete.** The page decides *Withdrawn* from each copy's own `pc-tu-withdrawn` tag, not
from the blocklist alone: a copy whose digest is blocklisted but which is not tagged yet shows
**Withdraw incomplete** (it may still be published there), with **Finish withdraw** enabled. That
action is the same Withdraw (the reason given then is not recorded again — the first one stands) and
finishes every provider that is reachable now; a blocklisted digest is never published, smoke-tested
or imported again meanwhile.

**The cloud-init drive of a Kubernetes node image is on `ide0`.** Only CAPMOX clones a node image. CAPMOX
v0.9.2 writes its NoCloud ISO to `ide0` and changes no other drive, so a drive on `ide0` is replaced by it,
as on a template of `images/k8s-node-template.sh`. A node image imported **before this change** has the
drive on `ide2`: a machine cloned from it starts with two media labelled `cidata`, cloud-init's NoCloud
source takes the later device, Proxmox's own drive, and the machine gets no kubeadm data and never joins
(read from the CAPMOX, go-proxmox and cloud-init sources; not observed on a lab, where the joins so far
used pipeline templates). Correct such a template once, on its node, before a cluster uses it:
`qm config <vmid> | grep cloudinit`, and when that names `ide2`: `qm set <vmid> --delete ide2`. Managed
services refuses such a template in stage 7 with the same command
([managed-services.md](managed-services.md#6-failure-codes), `machine_failed`). A tenant cluster's
provider has no such check of its own: the smoke test is the gate, see the end of this paragraph. A template corrected that way has no cloud-init drive at all. CAPMOX
needs none, and the smoke test adds one to its own clone (step 3 of the smoke test), so the template
can still be tested and published. Until 2026-11-16 the smoke test did not, and such a template
always failed `cloudinit_failed` with `cloud-init ended "disabled"`. The smoke test also
checks the slot: a node template that still has its drive on another slot than `ide0` is FAIL
`template_mismatch` with the same `qm set <vmid> --delete <slot>` command, before anything is cloned,
so it cannot be published to tenant clusters. A version published **before this check** from such a
template stays published: correct the template as above (the publish does not need to be repeated).

**Kubernetes node images.** The kamaji provider's `config.cluster.versions` names template VMIDs
explicitly, so withdrawing does **not** stop new workers from cloning a withdrawn image. The result
carries, per provider, a warning and the replacement line (`"1.35.10": "pve1=9210"` — the newest
other published image of that version per node), or a warning that none is left (remove the version
from the map or publish another image first). The portal does not edit the map itself.

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
prunes (a) a template carrying the import marker `pc-tu-import` **or any `pc-src-` tag** (an import made
before the marker existed) — the portal owns its lifecycle — and (b) the newest published version:
`--keep` must be 1–999 (refused by the CLI otherwise; the installer's preflight checks
`proxcloud.templates.keep` the same way, default 2). Before this change `--keep 0` pruned nothing (a
Python `[:-0]` slice); it is now refused rather than silently meaning "keep everything". Imports still count as versions for `--if-missing` (stage 20 skips building when an imported
version is published there) and take no `--keep` slot (pipeline versions keep K slots of their own).
Staged and withdrawn imports carry no `pc-public`/`pc-img-` at all, so `build.py` never even lists them.
A pipeline build publishes `pc-public` as before, so it can become the default over an older import
(newest `pc-v-` wins) — expected.

## 5. Sandbox templates — mirror, prepull, switch

Approved by the owner (2026-11-13, automatic). Code: [`internal/templateupdates/sandbox*.go`](../internal/templateupdates),
[`internal/cloud/sandbox/mirror.go`](../internal/cloud/sandbox/mirror.go),
[`internal/sandboximport/mirror.go`](../internal/sandboximport/mirror.go), admin routes in
[`templateupdates_sandbox.go`](../internal/platform/admin/templateupdates_sandbox.go), installer stages 55/60.

| Route | Permission | Flag off |
|---|---|---|
| `POST /admin/template-updates/sandbox` `{serviceId, name, digest, confirmDigest}` | `admin:service:manage` | 501 |
| `POST /admin/template-updates/sandbox/rollback` `{serviceId, name, confirmDigest}` | `admin:service:manage` | 501 |
| `GET /admin/template-updates/sandbox/state` (jobs, pins, prepull/adoption progress) | `admin:service:read` | answers |
| `GET /admin/template-updates/sandbox/pins[?registry=host]` (installer stage 55) | `admin:service:read` | answers |
| `DELETE /admin/template-updates/sandbox/pins/{id}` (id URL-escaped; images.yaml wins again) | `admin:service:manage` | answers |

### 5.1 Registry account `mirror`

Stage 55 generates `generated/sandbox-registry-mirror` next to `gc` and puts it in the registry's users
Secret; the account sync (`sandbox-proxy registry-users`) creates the Zot user only when that password is
mounted. Its rights, in `http.accessControl`:

| Pattern | Rights |
|---|---|
| `library/sandbox-*` (explicit, the most specific match) | group `pull`: read; user `mirror`: **read, create** |
| `library/**` | group `pull`: read (no `mirror`) |
| `t/**`, `t/<project>/**` | no `mirror` |

No `update` (an existing tag never moves), no `delete`, nothing under `t/**` or `library/proxcloud-*`;
`mirror` is not in the pull group nor an admin. **Residual risk**: `create` on the pattern lets the account
create a *new* repository `library/sandbox-<anything>` (Zot patterns cannot list names). Nothing pulls an
unknown name — the prepull DaemonSets and templates name only what the portal and `images.yaml` pinned, and
the admission policy keeps each init container in its own repository — but a leaked mirror password could
add images there (it could not overwrite or delete one). Stage 60 hands the password to the sandbox provider as
`secret.registryMirror` (encrypted, write-only like every provider secret: no read, list or audit returns
it). Idempotent: a provider registered after the change gets it with its other secrets; an existing one
gets one `PUT` whose hash (provider id + password, never the password) is kept in
`generated/sandbox-registry-mirror.handed`, so a re-run writes nothing unless the password changed. A
provider whose release gates do not hold refuses the write (409): reported, retried on the next run.

### 5.2 The mirror job

`templateupdate.sandbox/v1`, an external durable kind with its own fenced runner (the same
lease/heartbeat/`Checkpoint`/`Fail` contract as the VM import, §3). Enqueue re-lists the package and
re-verifies the digest server side (a published `<YYYYMMDD>-<sha7>` tag whose manifest is a linux/amd64
**image manifest** — an index digest cannot be copied byte for byte and is refused), refuses when the
prepull DaemonSets already run that digest (**same-digest refusal**, 409), and in one transaction under
an advisory lock on `<registry>/library/sandbox-<name>` refuses while another job for that image is not
settled (**dedup per image**, 409) before it enqueues the job and its receipt. A job is settled when its
receipt is `succeeded` or cleaned up, or its durable job `succeeded`: a receipt already `failed` whose job
is still running (the failure is recorded before the job's cleanup runs) keeps the image busy, and so
does a `dead` job whose receipt is not cleaned up yet (its cleanup is still to come from the dead-job pass),
so nothing overlaps that cleanup. The pass ends by marking the receipt cleaned (with the reason when cleanup is
impossible, e.g. the provider is gone); while the provider's cluster cannot be reached or a store write fails
it retries every 10 minutes and says so on the receipt (*cleanup waiting: …*), and the 409 names that job. Receipt steps (collection `templateUpdateSandbox`):

`queued → mirroring (intent: pod name) → mirrored → verified → prepull (intent: pin + previous images) → pinned → succeeded | failed`

1. **Copy**: the trusted `sandbox-import` worker in copy mode — fixed argv
   `mirror ghcr.io/mutex-solutions/sandbox-<name>@<digest> <registry>/library/sandbox-<name>:<tag> <policy>`,
   the operator-pinned digest image of `sandbox-system/sandbox-import-worker` (read through a Role that
   allows `get` on that one Secret), a pod in `sbx-build`
   (`tu-mirror-<16 hex>-e<epoch>`, no service-account token, read-only root, its image pulled with the
   read-only `sandbox-registry-nodes` account the sync now also writes into `sbx-build`). Its two
   credentials are dockerconfigjson Secrets written **after** the pod, with an `ownerReference` to the
   pod's observed UID: an **anonymous short-lived GHCR registry token** pull-scoped to the one
   `sandbox-<name>` package — the mirror pulls the **public** `sandbox-*` packages anonymously: the portal
   requests a per-repository pull token **without any `Authorization` header**
   (`GET https://ghcr.io/token?scope=repository:mutex-solutions/sandbox-<name>:pull&service=ghcr.io`) and
   confirms it reads that package. **The PAT is used only for discovery (GitHub REST listing) and never leaves
   the portal**: it is never sent to GHCR for this token, and there is no fallback to it. Before the token is
   written anywhere it is checked: non-empty, ≤ 8 KiB, printable ASCII without whitespace, not a GitHub token
   (`ghp_`, `github_pat_`, …), not equal to / containing the PAT or its base64 form, no base64 decoding of it
   (any alphabet, also after a `v1:`-style prefix) containing the PAT, and an `expires_in`, if present, in
   1..3600 s — otherwise the job fails permanently with `ghcr_token_unscoped` (the message never carries the
   token or the PAT). **Private packages are not supported by the mirror**: a package GHCR does not serve
   anonymously (401/403/404 from `/token` or the read) fails permanently with `ghcr_package_not_public`.
   The token cannot be refreshed inside the worker: a copy that outlives it (or any other worker failure)
   ends the pod `Failed`, which is **retryable** — the receipt records the fixed warning code
   `mirror_worker_failed`, the job is retried with backoff, and the next attempt mints a fresh anonymous
   token for its new pod (until `attempts_exhausted`). The worker's transport only lets through a `/token`
   request with exactly `scope=repository:mutex-solutions/sandbox-<name>:pull` and `service=ghcr.io`.
   The worker refuses a username/password for the source — and the mirror account (the registry only). They are garbage-collected
   with the pod; the job deletes the pod (UID precondition) as soon as the result is read, and every pod of
   the job on finish/cleanup. A failed delete is recorded in the receipt. A pod that stays Pending more than
   5 minutes is replaced; a pod whose Secrets were not confirmed written is never waited for. The runner
   runs even with the flag off, but then never runs a job (no worker, no prepull patch): a job it can
   claim — queued (a retry once it is due), or running with an **expired** lease (its runner is gone) —
   ends `failed` `feature_disabled` (or with the failure code an earlier attempt already recorded) with its
   cleanup (its worker pods and their Secrets deleted, the pin
   of a prepull switch that never landed restored), so it no longer blocks its image or
   `DELETE …/pins/{id}`; one whose switch already landed (`pinned`) is acknowledged as succeeded. A
   running job whose lease is still valid (another replica with the flag on) is never touched. A running
   job whose lease expired on its **last** attempt goes straight to `dead` (the durable store's own rule)
   and is cleaned up by the dead-job pass instead (`job_dead`). It also
   cleans up dead jobs and deletes mirror pods of jobs that are not pending/running (an **orphan sweep**,
   every 10 minutes, every sandbox provider), so no pod with a credential outlives its job. The worker
   copies the source's own manifest bytes
   (same digest) through transports fenced to `ghcr.io` (pull scope of that one repository; the blob
   redirect to `pkg-containers.githubusercontent.com` without credentials) and to the one destination
   repository; an existing destination tag is accepted only at the same digest (a re-run), never moved.
   Its single stdout line must prove `destinationDigest == sourceDigest` for exactly its pair.
2. **Verify**: the portal itself reads the destination manifest by tag with the mirror account (read)
   and its CA, and requires the sha256 of the bytes to be the payload digest (`digest_mismatch`,
   permanent, otherwise).
3. **Prepull**: in one fenced transaction the pin record (`sandboxBuiltinPin`, below) and the images
   being replaced are recorded; then the DaemonSets are patched (§5.3). Each DaemonSet is its own
   guarded patch: if one lands and the other is refused, the landed containers are recorded and the
   attempt retries; a job that ends that way puts the landed containers back on their recorded images
   (only those still on its ref) before it restores the pin, so the two DaemonSets never stay on
   different digests.

Failure codes (`[a-z_]`, never a provider string): `invalid_payload`, `receipt_mismatch`,
`github_not_configured`, `ghcr_package_not_public`, `ghcr_token_unscoped`, `mirror_unavailable` (no worker image, no template registry or no mirror
account), `mirror_result_invalid`, `digest_mismatch`, `registry_refused`, `rollback_target_missing`,
`ownership_lost`, `provider_missing`, `registry_changed`, `executor_unavailable`, `attempts_exhausted`,
`job_dead`, `feature_disabled` (ended because the flag was off). **Resume**: a new acquisition
continues from the last committed step; a worker that already succeeded is reused (no second copy), one
still running is waited for, a failed one is deleted and the copy redone under the new epoch's pod. A lost lease stops the executor before its next cluster effect.
**Cleanup** (terminal failure or the dead-job pass): delete the job's worker pods; if the pin was
recorded but the DaemonSet never ran the new ref, restore the pin record this job replaced. Cleanup
needs only the provider's cluster (its `secret.kubeconfig`): it gets its own executor, without the mirror
account, the registry reader or the GitHub token, so a job that failed because the provider lost its
mirror account (`mirror_unavailable`) or moved its template registry (`registry_changed`) is still
cleaned up. Only a provider that no longer exists marks the cleanup impossible (a receipt warning); an
unusable kubeconfig is retried by the dead-job pass every 10 minutes. **Secrets**:
the PAT never reaches the sandbox cluster (nor the payload, receipt, pin, audit row or log); the anonymous
pull token exists only in the job's memory and the pod-owned Secret; the mirror password only in the
provider's encrypted secret and the pod-owned Secret.

### 5.3 Prepull RBAC and admission policy

Stage 55 renders, next to the DaemonSets (`templates/sandbox-prepull.yml.j2`):

- `Role proxcloud-template-prepull` in `sandbox-system`: `get`, `patch` on `apps/daemonsets` with
  `resourceNames: [template-prepull, template-prepull-gpu]`, bound to the portal's sandbox identity —
  ServiceAccount `proxcloud` in `proxcloud-system` (`deploy/sandbox-cluster/rbac.yaml`; its token is the
  sandbox provider's `secret.kubeconfig`, stage 55 `sandbox-portal.kubeconfig`).
- `Role proxcloud-import-worker` in `sandbox-system`: `get` on the one Secret `sandbox-import-worker`
  (the trusted worker's image ref, no credential), same subject.
- `ValidatingAdmissionPolicy proxcloud-template-prepull-images-only` + binding (`Deny`, `failurePolicy:
  Fail`), matching that identity's DaemonSet CREATE/UPDATE/DELETE: only an UPDATE of those two names in
  `sandbox-system`; metadata (labels, owners, finalizers, annotations but the API server's
  `deprecated.daemonset.template.generation`), the DaemonSet spec, the pod template metadata and every pod
  spec field but `initContainers` unchanged; no init container removed, at most one added; each init
  container unchanged, or changed in `image` alone to a digest ref **of the same repository**
  (`<registry>/library/sandbox-<name>[:<YYYYMMDD>-<sha7>]@sha256:<64 hex>`), or one new `p<N>` equal to an
  existing one but for name and image (same grammar) that carries the hardened `securityContext`
  (`runAsNonRoot`, `runAsUser: 65532`, no privilege escalation, read-only root, `drop: [ALL]`). Every
  prepull init container is rendered with that `securityContext` (they only run `sh -c true`). "The same
  repository" is the whole repository name: `sandbox-base` never moves to `sandbox-base-evil` (or to a
  prefix of an over-long name).

**Order: the policy before the grant.** Stage 55 applies the template in two parts. First everything but the
portal's patch right (the ConfigMap, the DaemonSet, the import-worker Role and binding, the policy and its
binding); then it probes the policy with a server-side dry run of a forbidden change (a label) as the
portal's identity, impersonating `system:masters` as well so RBAC cannot be what refuses it (on a first
install the grant does not exist yet). Only once the policy refuses the probe (bounded: 30 tries, 2 s apart)
does it apply `Role`/`RoleBinding proxcloud-template-prepull`. A policy that does not take effect in time
deletes the RoleBinding (revoking an earlier grant too) and stops the stage; the mirror cannot move images
until `make upgrade` passes. So the portal never holds the patch right without the policy in force.

**Residual risk, not new with the mirror:** `deploy/sandbox-cluster/warming-rbac.yaml` (optional image
warming, operator-applied, off by default) grants the same ServiceAccount `create`/`patch`/`delete` on pods
and `create`/`patch` on Secrets in `sandbox-system`. Where an operator applied it, that identity can already
run a pod of its choosing there, which is a wider right than the prepull patch the policy narrows; the policy
does not fence those writes, and the mirror adds nothing to them. Leave image warming off unless its own
review says otherwise.

**Kubernetes CEL cannot iterate over the fields of a typed object, so the policy compares fields BY NAME
— and is fail-OPEN for a field it does not name**: a field the cluster has and the lists lack would be
writable by the portal. The lists live in one place, `pc_prepull_policy_fields`
(`installer/playbooks/group_vars/all/prepull-policy.yml`), used by the template and by a stage-55 check
that runs **before** the policy is applied: it reads `/openapi/v3/api/v1` and `/openapi/v3/apis/apps/v1`
from the sandbox cluster and stops the upgrade if `ObjectMeta`, `PodSpec`, `Container`, `DaemonSetSpec` or
`PodTemplateSpec` has any property the policy does not compare, or one of them is not served. The
deliberately uncompared properties are listed under `uncompared` in the same file, each with its reason:
`initContainers`, container `name`/`image`, `template`, `spec`, and the ObjectMeta fields a rule of its own
checks (`annotations`) or the API server owns (`name`, `namespace`, `uid`, `resourceVersion`, `generation`,
`managedFields`, the timestamps, …). A listed field the cluster lacks is harmless (`has()` is
false on both sides). **The check runs only at stage 55**: after upgrading the sandbox cluster's Kubernetes
outside `make upgrade` (by hand, or by the distribution's own upgrade), run stage 55 again (`make upgrade`)
right away. Until then a field the new version added is not compared, so the portal could change it; the
check then fails until the field is added to the lists.

The job's patch (`SetPrepullImage`): every init container of either DaemonSet that pulls
`<registry>/library/sandbox-<name>` gets the new ref; if none does, one `p<N>` (the installer's naming)
shaped like the first one is appended to `template-prepull`. A JSON merge patch carrying the
`resourceVersion` it read (409 if the DaemonSet moved), nothing else.

**Admission policy test cases**, evaluated offline by the API server's own CEL admission libraries
(`k8s.io/apiserver` v0.35, `test/policy`, its own Go module so the API does not depend on it) against the
rendered policy, at the API server's own per-binding runtime cost budget (`RuntimeCELCostBudget`, 1e7), and
failing on any evaluation error (so a deny never hides a broken expression) — run by
`installer/tests/test_sandbox_mirror.py` (`make test`) and CI:
allow an image change of `p1` to a mirrored digest ref of its own repository (with or without the tag); allow
adding a hardened `p<N>` shaped like the others; allow the template-generation annotation to move; deny a
tag-only ref, a ref under `t/`, `library/proxcloud-*`, another registry or `ghcr.io`; deny another
repository in an existing container (also re-pointing `python`); deny changing `command`, `env`,
`securityContext`, `volumeMounts`, `restartPolicyRules`; deny removing an init container, adding two,
adding one without the hardened `securityContext` or not named `p<N>`; deny changing `containers`,
`volumes`, `serviceAccountName`, `hostNetwork`, `hostnameOverride`, `tolerations`, `nodeSelector`,
`imagePullSecrets`, a label, an annotation, the selector or the pod template labels; deny another DaemonSet
or namespace, CREATE and DELETE; deny a repository name that extends or prefixes the old one (also an
over-long old name), another sandbox repository in place, look-alike registries (a longer address, a
subdomain, a port), 63/65-hex, upper-case or trailing-junk digests and a free-form tag; deny renaming `p1`,
a toleration's key, the pod `securityContext`, a nested init-container `securityContext` change, an added
`p2` with a `seccompProfile`, with `privileged: false` spelled out, with a tag-only ref, from another
registry or with another command; deny a pod template annotation, finalizers, ownerReferences,
`updateStrategy.type` and an extra annotation beside the template-generation one; allow swapping the init
containers' order (each only pulls its image and exits); a DaemonSet write by any other identity is not
matched. Removing one
compared field from the rendered policy makes its case fail (checked while writing the test).

### 5.4 Switch and rollback

No new switch code: the existing `sandboxTemplateAdopt` job moves each template (`SetTemplateImage` CAS)
once every prepull pod on a Ready node has pulled the new digest (`imageAdoption: on` on the provider;
`dry-run` only logs). The page shows *Mirroring → Verifying digest → Updating prepull → Waiting for
prepull on N/M nodes → Switched* (`GET …/sandbox/state`). **Rollback** enqueues the same job kind with
action `rollback` to the pin's `previous` ref (`<registry>/library/sandbox-<name>:<tag>@<digest>`): it
re-verifies that digest is still in the registry (nothing deletes it: Zot GC removes only unreferenced
blobs and the old tag still references them), records the pin and re-patches the DaemonSets; adoption
follows. A `previous` that is not such a ref (e.g. the first pin of an image the installer pinned without
a `<YYYYMMDD>-<sha7>` tag) is refused: roll back through `images.yaml` and `make upgrade`.

### 5.5 Pin record and `make upgrade`

`sandboxBuiltinPin` (id `<registry>/library/sandbox-<name>`): `{template, registry, tag, digest, ref,
action, previous, serviceId, actor, at, mirrorJob}`. Stage 55 reads the records **through the portal API**
(`GET /admin/template-updates/sandbox/pins?registry=<host>`, read-only, secret-free, with the installer's
admin API key `generated/admin-api-key.json` that stage 60 already uses) — chosen over a database read,
which would need the database password and a path into the mgmt cluster's PostgreSQL. It then renders the
copy list and the prepull from the **newer** of `images.yaml` and the pin per image: same digest → no
change; a pin whose `previous` is exactly `images.yaml`'s digest wins (a newer mirror, or a deliberate
rollback); otherwise the later `<YYYYMMDD>` tag date wins; two digests that cannot be ordered stop the
upgrade with the line to commit. It prints the `images.yaml` lines to commit; the page shows the same line
per pin. To let `images.yaml` win for an image on purpose, delete its pin (`DELETE …/sandbox/pins/{id}`,
`admin:service:manage`, audited, refused (409) while a job for the image is not settled, the same rule
as the dedup in §5.2; the cluster is not touched).
**Fail closed**: the upgrade stops with a clear message instead of rendering `images.yaml` alone when the
admin key is present and the API does not answer, refuses, or answers anything but `{data: {pins: [...]}}`
(an older API, a proxy page); and when `generated/sandbox-registry-mirror.handed` exists (the portal got
the mirror account) but the admin key is missing. `-e pc_sandbox_portal_pins_unavailable_ok=true`
overrides it only when the operator knows no image was mirrored from the portal. No admin key and no
hand-over = first install: nothing to read.

### 5.6 Enabling

1. `make upgrade` (stages 55 and 60): the mirror account, its hand-over, the Role and the policy.
2. Check: `kubectl -n sandbox-system auth can-i patch daemonset/template-prepull
   --as=system:serviceaccount:proxcloud-system:proxcloud` → yes; `… patch daemonset/node-setup` → no; the
   policy is admitted (`kubectl get validatingadmissionpolicy proxcloud-template-prepull-images-only`).
3. `sandbox-system/sandbox-import-worker` (key `image`) holds the digest-pinned worker built from this
   release (`deploy/sandbox-import.Dockerfile`, pushed to `<registry>/library/proxcloud-sandbox-import`;
   copy mode and its registry-token source are new: an older worker refuses them). The installer grants the
   portal `get` on that one Secret (Role `proxcloud-import-worker`) but does not build or write it; check
   with `kubectl -n sandbox-system auth can-i get secret/sandbox-import-worker
   --as=system:serviceaccount:proxcloud-system:proxcloud` → yes, `… get secret/sandbox-registry-nodes` → no.
4. The provider has `imageAdoption: on` and the GitHub integration is configured.
5. `PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true` (or `proxcloud.template-updates.sandbox-enabled`) on
   the API: starts the runner and allows the two POSTs. Off, they answer 501 and nothing runs. Set it the
   same on every API replica: one with it off ends the queued sandbox jobs it claims (`feature_disabled`).

## 6. Permissions

| Action | Permission | Built-in roles |
|---|---|---|
| See the page, discovery, placement, history, template states, blocklist, the publish-on-all-nodes report | `admin:service:read` | SUPER_ADMIN, ADMIN, SUPPORT |
| Start a VM import, run a smoke test, publish, publish on all nodes, withdraw | `admin:service:manage` | SUPER_ADMIN, ADMIN |
| Start a sandbox mirror or rollback, delete a sandbox pin | `admin:service:manage` | SUPER_ADMIN, ADMIN |
| Sandbox state, pin list (installer stage 55) | `admin:service:read` | SUPER_ADMIN, ADMIN, SUPPORT |
| Save the GitHub integration | `admin:integration:manage` | SUPER_ADMIN, ADMIN |

Proxmox rights of the portal token for the smoke test, on `/pool/pc-templates` (already in
`ProxCloudPlatform` on `/pool`): `VM.Clone`, `VM.Allocate`, `VM.Config.Network`/`Cloudinit`/`Disk`/
`Options`, `VM.PowerMgmt`, `VM.GuestAgent.Audit` (ping, osinfo, fsinfo) and
**`VM.GuestAgent.Unrestricted`** (guest-exec: `cloud-init status`, `df`, `kubelet --version`; without it
the smoke ends FAIL `guest_exec_forbidden`); `Datastore.AllocateSpace` on the template disk storage;
`SDN.Use` on the smoke bridge's zone (`/sdn/zones/localnetwork/<bridge>` for a plain bridge on PVE 8+).
Publish/withdraw need `VM.Config.Options` (tags) only. An existing site whose `ProxCloudPlatform` role
predates `VM.GuestAgent.Unrestricted` adds it as described in
[proxmox-permissions.md](proxmox-permissions.md#proxcloudplatform) (`pveum role modify ProxCloudPlatform
--append 1 --privs "VM.GuestAgent.Unrestricted …"`); the smoke network's bridge needs `SDN.Use` as for
any NIC ([proxmox-permissions.md](proxmox-permissions.md#pveum-commands)).

## Known issue: Ubuntu 26.04 images exported before the orphan-file fix

Every `proxcloud-vm-ubuntu-2604` and `proxcloud-vm-ubuntu-2604-k8s` artifact exported before the fix has a
stale field in its ext4 superblock (cause, pipeline fix and the list of affected tags and digests:
[public-vm-sanitization.md](public-vm-sanitization.md#known-issue-stale-orphan-file-inode-in-ubuntu-2604-images-published-before-the-fix)).
A VM made from one can get `EXT4-fs error ... ext4_lookup ... EFSCORRUPTED` and a read-only root at a
reboot, once inode 12 holds a file. Affected: every template imported from one of the listed digests (the
**Imported templates** tab shows the digest; the name `pc-ghcr-ubuntu-2604-*` alone does not tell, fixed
images get the same name) and every server or Kubernetes node cloned from one. `proxcloud-vm-ubuntu-2404`
images are not affected.

**Do not reboot a VM that has logged the error** (`dmesg` shows the line above, or the root is read-only).
Shut it down and repair it offline first. Its filesystem is marked as having errors, so the next boot runs
`e2fsck -p` on it. On a scratch image in that state `e2fsck -p` cleared the file in inode 12, left the field
in place and exited 4 (`UNEXPECTED INCONSISTENCY; RUN fsck MANUALLY`); on Ubuntu that exit status on the root
stops the boot in the initramfs shell. The offline repair below still applies after that, but the file that
was in inode 12 is gone. Not measured on a real VM, and the image's fstab was not read.

**Templates: replace, do not repair in place.** The fixed images are published (exported 2026-11-16):
`ubuntu-2604-os-20261116170652-72f0eaa3d0d4` in `proxcloud-vm-ubuntu-2604`, and `ubuntu-2604-k8s-1.35.10-20261116181221-72f0eaa3d0d4`,
`ubuntu-2604-k8s-1.36.6-20261116194043-72f0eaa3d0d4` and
`ubuntu-2604-k8s-1.37.2-20261116210716-72f0eaa3d0d4` in `proxcloud-vm-ubuntu-2604-k8s`
(digests: the same section of public-vm-sanitization.md). Do not publish an affected template to tenants.
For each affected template:

1. Import the fixed tag and let its smoke test pass.
2. Publish it. An OS template becomes what new servers get; for a Kubernetes node image put the version-map
   line the row shows on the Kubernetes provider.
3. Withdraw every affected digest you had imported (§4): it leaves the catalog on every provider and can
   not be imported again. The template VM is kept; remove it yourself when no linked clone needs it.

A withdraw changes no existing VM: every server and node cloned from an affected template still needs the
test and the repair below. A repair of the template disk was not tested, the
partition device nodes of a template zvol did not exist on the lab host, and a linked clone is made from the
template's base snapshot, which a repair of the live disk does not change (PVE behaviour, not verified here).
Until the template is replaced, repair each VM made from it before its second boot.

**Test**, as root inside the guest (reads only). The same reads were checked against image files; the loop
was not run inside a guest. From the PVE host the same two reads work on the partitions of the VM's disk, also
while the VM runs: replace the `findmnt` list by the partition devices, on ZFS
`/dev/zvol/<pool>/vm-<id>-disk-0-part1 /dev/zvol/<pool>/vm-<id>-disk-0-part13` (run so on a lab on 2026-11-17):

```sh
for d in $(findmnt -rn -t ext4 -o SOURCE | sort -u); do
  f=$(dd if=$d bs=1 skip=1116 count=4 2>/dev/null | od -An -tx4 | tr -d ' ')
  i=$(dd if=$d bs=1 skip=1664 count=4 2>/dev/null | od -An -tu4 | tr -d ' ')
  if [ $((0x$f & 0x1000)) -eq 0 ] && [ "$i" != 0 ]; then echo "AFFECTED $d s_orphan_file_inum=$i"; else echo "ok $d"; fi
done
```

**Repair** of a VM disk, only on an UNMOUNTED filesystem: shut the VM down and run it on the PVE host, or
from a rescue system, once for **every** partition the test reported as AFFECTED. For ZFS the partition of
`/dev/sda1` is `/dev/zvol/<pool>/vm-<id>-disk-0-part1`; the OS image also has `/boot` on `-part13`, the
Kubernetes node image has one ext4 partition.

```sh
D=/dev/XXX
e2fsck -E journal_only -p "$D"
printf 'ssv orphan_file_inum 0\nclose -a\n' > /tmp/orphan-fix.cmd
debugfs -w -f /tmp/orphan-fix.cmd "$D"
[ "$(dd if="$D" bs=1 skip=1664 count=4 2>/dev/null | od -An -tu4 | tr -d ' ')" = 0 ] && e2fsck -f -p "$D"
```

- The order matters. `e2fsck -E journal_only` replays the journal and checks nothing else. A VM whose root
  went read-only could not finish its journal, and without the replay the full check replays it first, which
  puts the old superblock back, and then clears the file in inode 12.
- `close -a` writes the field to the backup superblocks too. Without it they keep 12, and a later
  `e2fsck -b <backup>` brings the field back and deletes the file.
- `debugfs` exits 0 even when `ssv` failed, so the last line reads the field back and runs the full check
  only when it is 0. If `e2fsck -f -p` does not run, stop: the field is still set.
- `e2fsck -f -p` exit status 0 or 1 is success (1: it corrected something); 4 or more is not.
- Tested on scratch image files with e2fsprogs 1.47.2, with a file in inode 12, in four states: clean,
  journal not replayed, marked with errors, and both. Each time the field became 0 in the primary and the
  backup superblocks, the file kept its content and `e2fsck -fn` was clean afterwards.
- Run once on a real VM on a lab, on 2026-11-17: the seed VM of a managed-services installation, on a zvol,
  root (`-part1`, read-only after the error, its journal left by the guest's kernel) and `/boot` (`-part13`),
  e2fsprogs 1.47.2. Every command exited 0, the file in inode 12 kept its content, and the VM booted with
  both filesystems read-write.
- **Not tested**: e2fsprogs 1.47.0 for the journal and `close -a` steps, a disk on a store other than ZFS,
  the Kubernetes node image on a real VM.
- **Start the VM afterwards** (`qm start <id>`); nothing does it for you.
- **Do not** run the repair on a mounted filesystem: `debugfs -w` also rewrites group descriptors.
- **Do not** run `e2fsck -y`/`-p` (other than `-E journal_only`) before the field is zero: on the scratch
  image it deleted the file in inode 12 and left the field in place. `tune2fs -O orphan_file` destroys that
  file too.
- There is no online repair. A root filesystem already read-only needs the offline path.

## 7. Open items

- **Publish on all nodes is partly lab-verified** (2026-11-17, main cb2f052): two advances each published one node
  whose smoke test had passed, the report and the versions line were right, and the smoke clone upgraded no package.
  Not run on a lab: an import started by it, a smoke test started by it, and so its one-job-at-a-time rule (both
  copies there carried a failed smoke test, which it does not repeat, and were smoke-tested from their rows, one
  after the other by hand); the `blocked` answer; the dialog. Its tests run on PostgreSQL with a fake Proxmox. It has no background
  driver: an operator who closes the dialog pauses it. It does not write `config.cluster.versions`. The one-job rule
  reads the import and smoke receipts of the provider, so a job started by hand in the moment between the check and
  the enqueue can still overlap; the provider's own smoke lock keeps two smoke tests apart in every case.

- **Sandbox mirror not cluster-verified**: no sandbox cluster, Zot, GHCR or PAT was used. Verify on the
  lab: the Zot rule set (`mirror` creates `library/sandbox-x:tag`, cannot push an existing tag, cannot
  delete, cannot touch `t/**` or `library/proxcloud-*`; the pull group still reads `library/sandbox-*`);
  that a GHCR blob redirect host is `pkg-containers.githubusercontent.com` (otherwise the worker's guard
  refuses the copy, fail closed); the admission policy compiles on the cluster's Kubernetes version and
  the cases in §5.3 behave as listed; the DaemonSet merge patch with `resourceVersion` passes the policy
  (the API server's own `deprecated.daemonset.template.generation` bump included); garbage collection of
  the pod-owned Secrets; the worker's `activeDeadlineSeconds` against a large image (the 30-minute
  budget, and the `ResponseHeaderTimeout` on GHCR blobs); an index-only published digest is refused at
  enqueue (the sandbox-templates workflow builds single-platform images today).

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
- **Catalog pruning by `images/build.py`**: see §4 — imports (`pc-tu-import` or any `pc-src-` tag) are
  never pruned and the only published version is never pruned; imports count for `--if-missing`. A
  template imported *and published* by the pre-lifecycle version is protected by its `pc-src-` tag; it
  can be smoke-tested and published through the lifecycle (its name and `pc-src-` tag prove it), which
  adds the marker.
- **Withdraw across providers is not atomic**: the blocklist entry is written first, then each provider's
  copies are retagged; a provider that is down keeps its copy published until the withdraw is repeated
  (the page shows *Withdraw incomplete*). A provider **deleted** from the portal while it still holds a
  published copy cannot be reached at all (`provider_missing`): retag that copy by hand.
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

## Console display

Up to v0.9.1 every template was made with `serial0: socket` and `vga: serial0`, and every clone
copied it (tenant servers, Kubernetes workers, the platform's machines and its seed). The display
of such a VM is its serial port, so the Proxmox web UI opens its console on the serial terminal
("starting serial terminal on interface serial0", an empty page until a key is pressed).

From v0.10.0 a template is made with `vga: std` and keeps `serial0: socket`
(`templateupdates.Executor.importDisk`, `pve.ImportImage`, `images/build.py`, and the cold-boot
clones of `images/vm_export/coldboot.py`):

- The **Console** button of the Proxmox web UI opens the graphical console (noVNC). The serial
  terminal is still there: **Console → xterm.js**, or `qm terminal <vmid>` on the node.
- The portal's own console page is the same page as before. **Graphical** goes through `vncproxy`,
  **Serial** through `termproxy`, which needs the serial port and not the display. The page opens
  on Serial when the display in effect is a serial port and on Graphical otherwise
  ([cloud-integration.md](cloud-integration.md), "Console state").
- In the guest, **not measured**: what the graphical console (tty1) and the serial terminal (ttyS0)
  each show is decided by the guest's kernel command line and its getty units. No build file of
  this repository sets a `console=` argument, a GRUB terminal or a serial getty (`images/`, the
  Packer scripts), so the vendors' own settings apply, and nobody read them from the published
  images or booted one for this release. Expected, not proven: the Ubuntu and Debian cloud images
  print the boot and give a login prompt on both. For a Kubernetes node image (an image-builder
  installer build) there is not even an expectation for the serial terminal. If a guest prints
  only on the serial port, its graphical console stays mostly empty and its serial terminal works
  as before.
- Nothing the portal reads from a guest goes through the display: the join log of a worker is read
  through the guest agent, and cloud-init's own logs are files in the guest.

**Templates and VMs from before v0.10.0.** Proxmox cannot change `vga` on a running VM: it keeps
the write as a **pending** change until the VM's next stop and start. The portal's VM inventory
and its fenced writes (volume attach, detach, resize, server resize and delete) refuse a VM that
has any pending change, so a pending `vga` would stop the project's sync and those operations.
**The portal therefore writes `vga` only where it lands at once, and never leaves a pending
change.** What it writes is what `qm set <vmid> --vga std` writes, where `vga` is a serial port;
`serial0` is not touched. No new Proxmox privilege (`VM.Config.HWType`, which `ProxCloudPlatform`
holds on `/pool`).

| What | When it changes | Ownership test |
|---|---|---|
| Templates of Template updates (OS and Kubernetes) | **at once**: the next services sync, per provider. A template does not run | an import of this portal for this provider still holds the template (the receipt: node, VM id, name, source digest). A **withdrawn** import (`pc-tu-withdrawn`) is neither read nor written |
| Stopped tenant servers, images a tenant imported | **at once**: the next services sync, per project | the VM is in the pool of a project of this portal, is stopped, has no lock and is not a Kubernetes worker |
| Running tenant servers | **at the next start from the portal**: Stop then Start, or any action of the portal that stops and starts the server (resize, rebuild, restore of a backup, rollback of a snapshot of a running server). The portal sets `vga: std` while the server is stopped, then starts it | the same pool test; the portal's start reads the VM's status first and writes only a stopped VM without a lock |
| Kubernetes workers (tenant clusters) | **when their node group is rolled** to a template that has `vga: std`: the new clones inherit it. The portal never writes a worker, running or stopped, and does not start workers (CAPMOX does) | none: not written |
| The platform's machines and the seed | **when they are replaced** by clones of a template that has `vga: std` (a platform upgrade that rolls the machines). **Apply this release** does not write `vga` on a running machine. The seed is the one the platform starts itself: when an install starts a stopped seed that still has a serial display, the same start sets `vga: std` first (CAPI token, `VM.Config.HWType` on the platform's pools) | the platform's own seed lookup; stopped and without a lock |

- A **Reboot** from the portal (soft or hard) and a reboot from inside the guest do not change
  the display: the VM is not stopped and started by the portal, and the portal does not write a
  running VM.
- The sync writes once per VM: a later sync finds `vga: std` and writes nothing. A failed write
  is logged (`imported templates start at boot and display`, `graphical display`) and tried again
  at the next sync. A failed write before a start is logged (`graphical display not set before
  the start`) and the VM starts as it is.
- Should someone start a VM in Proxmox between the portal's status read and its write, the write
  is pending: the portal sees that the values in effect still show the serial port, takes the
  change back (`revert`) and tries again at the next sync or start.
- **By hand**, for a running server, a worker or a platform machine that should change now:
  `qm set <vmid> --vga std` on its node, then **stop and start** the VM (`qm shutdown <vmid>`,
  `qm start <vmid>`; a reboot is not enough). Between the `qm set` and the stop, the VM has a
  pending change: its project's sync and the volume and server operations on it are refused until
  the VM has been stopped and started (or the change taken back with `qm set <vmid> --revert vga`).
  Do the three steps together. Drain a Kubernetes worker first.
- Not covered by the sync: a template no receipt of this portal holds (built by
  `images/build.py`, the site's own, another portal's) and a withdrawn template. Run
  `qm set <vmid> --vga std` on those; a template is stopped, so it lands at once.
- The sync sets `vga: std` again on a stopped VM in a tenant pool that someone put back on a
  serial display by hand, and so does a start from the portal. There is no setting to keep one VM
  on it.
- Tested against a fake Proxmox API that models pending changes; not yet run on a lab.
