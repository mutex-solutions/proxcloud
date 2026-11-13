# Template updates from GitHub Packages

**Admin → System → Template updates** shows the newest VM templates and sandbox images published on
GitHub Packages (`ghcr.io/mutex-solutions`), compares them with what this site runs, and — for VM
templates — imports a chosen artifact as a **new** Proxmox template; for sandbox images — mirrors the
chosen digest into the sandbox registry and pre-pulls it before templates switch. Scope chosen by the
owner (2026-11-12): *the admin sees the latest available version and clicks update*. Nothing updates
on its own.

| Part | State |
|---|---|
| GitHub integration (PAT, validated on save) | done |
| Discovery: newest VM artifact per line, newest sandbox image per template, installed/pinned state | done |
| VM import job (download by digest → verify → new template) | done, **off by default** (`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`) until lab-verified |
| Sandbox mirror → prepull → pin switch from the portal (+ rollback) | done, **off by default** (`PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true`) until cluster-verified (§4) |

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
runner and allows enqueueing. Off: discovery still works and the import button is disabled. Before
turning it on, run one import per OS on the lab (PVE 9.2) and boot a clone: see *Open items*.

## 4. Sandbox templates — mirror, prepull, switch

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

### 4.1 Registry account `mirror`

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

### 4.2 The mirror job

`templateupdate.sandbox/v1`, an external durable kind with its own fenced runner (the same
lease/heartbeat/`Checkpoint`/`Fail` contract as the VM import, §3). Enqueue re-lists the package and
re-verifies the digest server side (a published `<YYYYMMDD>-<sha7>` tag whose manifest is a linux/amd64
**image manifest** — an index digest cannot be copied byte for byte and is refused), refuses when the
prepull DaemonSets already run that digest (**same-digest refusal**, 409), and in one transaction under
an advisory lock on `<registry>/library/sandbox-<name>` refuses while another job for that image is live
(**dedup per image**, 409) before it enqueues the job and its receipt. Receipt steps (collection
`templateUpdateSandbox`):

`queued → mirroring (intent: pod name) → mirrored → verified → prepull (intent: pin + previous images) → pinned → succeeded | failed`

1. **Copy**: the trusted `sandbox-import` worker in copy mode — fixed argv
   `mirror ghcr.io/mutex-solutions/sandbox-<name>@<digest> <registry>/library/sandbox-<name>:<tag> <policy>`,
   the operator-pinned digest image of `sandbox-system/sandbox-import-worker` (read through a Role that
   allows `get` on that one Secret), a pod in `sbx-build`
   (`tu-mirror-<16 hex>-e<epoch>`, no service-account token, read-only root, its image pulled with the
   read-only `sandbox-registry-nodes` account the sync now also writes into `sbx-build`). Its two
   credentials are dockerconfigjson Secrets written **after** the pod, with an `ownerReference` to the
   pod's observed UID: a **short-lived GHCR registry token** pull-scoped to the one `sandbox-<name>`
   package — the portal exchanges the GitHub PAT for it (`GET https://ghcr.io/token?scope=repository:
   mutex-solutions/sandbox-<name>:pull`); **the PAT never leaves the portal** and the worker refuses a
   username/password for the source — and the mirror account (the registry only). They are garbage-collected
   with the pod; the job deletes the pod (UID precondition) as soon as the result is read, and every pod of
   the job on finish/cleanup. A failed delete is recorded in the receipt. A pod that stays Pending more than
   5 minutes is replaced; a pod whose Secrets were not confirmed written is never waited for. The runner
   runs even with the flag off (it then claims no job): it cleans up dead jobs and deletes mirror pods of
   jobs that are not pending/running (an **orphan sweep**, every 10 minutes, every sandbox provider), so no
   pod with a credential outlives its job. The worker copies the source's own manifest bytes
   (same digest) through transports fenced to `ghcr.io` (pull scope of that one repository; the blob
   redirect to `pkg-containers.githubusercontent.com` without credentials) and to the one destination
   repository; an existing destination tag is accepted only at the same digest (a re-run), never moved.
   Its single stdout line must prove `destinationDigest == sourceDigest` for exactly its pair.
2. **Verify**: the portal itself reads the destination manifest by tag with the mirror account (read)
   and its CA, and requires the sha256 of the bytes to be the payload digest (`digest_mismatch`,
   permanent, otherwise).
3. **Prepull**: in one fenced transaction the pin record (`sandboxBuiltinPin`, below) and the images
   being replaced are recorded; then the DaemonSets are patched (§4.3). Each DaemonSet is its own
   guarded patch: if one lands and the other is refused, the landed containers are recorded and the
   attempt retries; a job that ends that way puts the landed containers back on their recorded images
   (only those still on its ref) before it restores the pin, so the two DaemonSets never stay on
   different digests.

Failure codes (`[a-z_]`, never a provider string): `invalid_payload`, `receipt_mismatch`,
`github_not_configured`, `github_refused`, `mirror_unavailable` (no worker image, no template registry or no mirror
account), `mirror_result_invalid`, `digest_mismatch`, `registry_refused`, `rollback_target_missing`,
`ownership_lost`, `provider_missing`, `registry_changed`, `executor_unavailable`, `attempts_exhausted`,
`job_dead`. **Resume**: a new acquisition continues from the last committed step; a worker that already
succeeded is reused (no second copy), one still running is waited for, a failed one is deleted and the
copy redone under the new epoch's pod. A lost lease stops the executor before its next cluster effect.
**Cleanup** (terminal failure or the dead-job pass): delete the job's worker pods; if the pin was
recorded but the DaemonSet never ran the new ref, restore the pin record this job replaced. **Secrets**:
the PAT exists only in the job's memory and the pod-owned Secret — never in the payload, receipt, pin,
audit row or log; the mirror password only in the provider's encrypted secret and the pod-owned Secret.

### 4.3 Prepull RBAC and admission policy

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
  prepull init container is rendered with that `securityContext` (they only run `sh -c true`).

**Kubernetes CEL cannot iterate over the fields of a typed object, so the policy compares fields BY NAME
— and is fail-OPEN for a field it does not name**: a field the cluster has and the lists lack would be
writable by the portal. The lists live in one place, `pc_prepull_policy_fields`
(`installer/playbooks/group_vars/all/prepull-policy.yml`), used by the template and by a stage-55 check
that runs **before** the policy is applied: it reads `/openapi/v3/api/v1` and `/openapi/v3/apis/apps/v1`
from the sandbox cluster and stops the upgrade if `PodSpec`, `Container`, `DaemonSetSpec` or
`PodTemplateSpec` has any property the policy does not compare (beyond the deliberate `initContainers`,
container `name`/`image`, `template`, `spec`). A listed field the cluster lacks is harmless (`has()` is
false on both sides). After a Kubernetes upgrade of the sandbox cluster, run `make upgrade`: a new field
fails the check until it is added to the lists.

The job's patch (`SetPrepullImage`): every init container of either DaemonSet that pulls
`<registry>/library/sandbox-<name>` gets the new ref; if none does, one `p<N>` (the installer's naming)
shaped like the first one is appended to `template-prepull`. A JSON merge patch carrying the
`resourceVersion` it read (409 if the DaemonSet moved), nothing else.

**Admission policy test cases**, evaluated offline by the API server's own CEL admission libraries
(`k8s.io/apiserver` v0.35, `test/policy`, its own Go module so the API does not depend on it) against the
rendered policy — run by `installer/tests/test_sandbox_mirror.py` (`make test`) and CI:
allow an image change of `p1` to a mirrored digest ref of its own repository (with or without the tag); allow
adding a hardened `p<N>` shaped like the others; allow the template-generation annotation to move; deny a
tag-only ref, a ref under `t/`, `library/proxcloud-*`, another registry or `ghcr.io`; deny another
repository in an existing container (also re-pointing `python`); deny changing `command`, `env`,
`securityContext`, `volumeMounts`, `restartPolicyRules`; deny removing an init container, adding two,
adding one without the hardened `securityContext` or not named `p<N>`; deny changing `containers`,
`volumes`, `serviceAccountName`, `hostNetwork`, `hostnameOverride`, `tolerations`, `nodeSelector`,
`imagePullSecrets`, a label, an annotation, the selector or the pod template labels; deny another DaemonSet
or namespace, CREATE and DELETE; a DaemonSet write by any other identity is not matched. Removing one
compared field from the rendered policy makes its case fail (checked while writing the test).

### 4.4 Switch and rollback

No new switch code: the existing `sandboxTemplateAdopt` job moves each template (`SetTemplateImage` CAS)
once every prepull pod on a Ready node has pulled the new digest (`imageAdoption: on` on the provider;
`dry-run` only logs). The page shows *Mirroring → Verifying digest → Updating prepull → Waiting for
prepull on N/M nodes → Switched* (`GET …/sandbox/state`). **Rollback** enqueues the same job kind with
action `rollback` to the pin's `previous` ref (`<registry>/library/sandbox-<name>:<tag>@<digest>`): it
re-verifies that digest is still in the registry (nothing deletes it: Zot GC removes only unreferenced
blobs and the old tag still references them), records the pin and re-patches the DaemonSets; adoption
follows. A `previous` that is not such a ref (e.g. the first pin of an image the installer pinned without
a `<YYYYMMDD>-<sha7>` tag) is refused: roll back through `images.yaml` and `make upgrade`.

### 4.5 Pin record and `make upgrade`

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
`admin:service:manage`, audited, refused while a job for the image is live; the cluster is not touched).
**Fail closed**: the upgrade stops with a clear message instead of rendering `images.yaml` alone when the
admin key is present and the API does not answer, refuses, or answers anything but `{data: {pins: [...]}}`
(an older API, a proxy page); and when `generated/sandbox-registry-mirror.handed` exists (the portal got
the mirror account) but the admin key is missing. `-e pc_sandbox_portal_pins_unavailable_ok=true`
overrides it only when the operator knows no image was mirrored from the portal. No admin key and no
hand-over = first install: nothing to read.

### 4.6 Enabling

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
   the API: starts the runner and allows the two POSTs. Off, they answer 501 and nothing runs.

## 5. Permissions

| Action | Permission | Built-in roles |
|---|---|---|
| See the page, discovery, placement, history | `admin:service:read` | SUPER_ADMIN, ADMIN, SUPPORT |
| Start a VM import | `admin:service:manage` | SUPER_ADMIN, ADMIN |
| Start a sandbox mirror or rollback | `admin:service:manage` | SUPER_ADMIN, ADMIN |
| Sandbox state, pin list (installer stage 55) | `admin:service:read` | SUPER_ADMIN, ADMIN, SUPPORT |
| Save the GitHub integration | `admin:integration:manage` | SUPER_ADMIN, ADMIN |

## 6. Open items

- **Sandbox mirror not cluster-verified**: no sandbox cluster, Zot, GHCR or PAT was used. Verify on the
  lab: the Zot rule set (`mirror` creates `library/sandbox-x:tag`, cannot push an existing tag, cannot
  delete, cannot touch `t/**` or `library/proxcloud-*`; the pull group still reads `library/sandbox-*`);
  that a GHCR blob redirect host is `pkg-containers.githubusercontent.com` (otherwise the worker's guard
  refuses the copy, fail closed); the admission policy compiles on the cluster's Kubernetes version and
  the cases in §4.3 behave as listed; the DaemonSet merge patch with `resourceVersion` passes the policy
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
