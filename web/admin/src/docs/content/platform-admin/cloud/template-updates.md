# Template updates

ProxCloud publishes its VM templates (Ubuntu, Debian and the Kubernetes node image) and its sandbox
images on GitHub Packages. **System > Template updates** shows the newest published version of each,
next to what your site runs, lets you import a newer VM template, and update a sandbox image.

## Connect GitHub first

GitHub limits anonymous API use, so the page works only with your own token.

1. On GitHub, create a **personal access token (classic)** with just the `read:packages` scope.
   Fine-grained tokens do not work with GitHub Packages.
2. In the admin portal go to **System > Integrations**, open **GitHub** and paste the token.
3. Save. The token is checked with GitHub right away; a token without `read:packages` is refused.

The token is stored encrypted and never shown again. To replace it, paste a new one; saving without a
token keeps the current one.

## What the page shows

- **VM templates** — one row per template line (an OS, or the Kubernetes node image of one Kubernetes
  version), the newest published version with its digest and size, and the templates each Proxmox
  provider already has. **Update available** means at least one provider has nothing that new.
  Versions that fail the published-artifact checks are never offered; the row says how many were refused.
- **Imported templates** — every VM template imported here, with its lifecycle state and actions (see below).
- **Sandbox templates** — the newest image of each built-in sandbox template, the digest each
  sandbox provider's templates use, and the sandbox update jobs.
- **Jobs** — every import you started and every smoke test, their progress and result.

Results are cached for two minutes; **Check now** asks GitHub again. The remaining GitHub API quota
is shown at the bottom.

## Importing a VM template

Imports must be enabled for your site by the operator (`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`);
until then the button is disabled. You need the *Manage external services* permission.

1. Choose **Import…** on a row.
2. Pick the Proxmox provider, the node, a storage with **import** content (where the download is
   placed) and the storage the template disk goes to. Only storages that are active on that node are offered.
3. The import always lands **staged**: tenants do not see it until you publish it (see below).
4. Paste the exact digest shown in the dialog to confirm, then **Import as new template**.

The download is checked three times (its SHA-256 while streaming, by Proxmox on upload, and against the
published digest), then imported as a **new** template VM in pool `pc-templates`, named
`pc-ghcr-<os>-…`. Existing templates are never changed or removed: remove old ones yourself when no
longer needed.

## Smoke test, publish, withdraw

The **Imported templates** tab shows each imported template's state: *Staged*, *Smoke running*,
*Smoke failed* (with the reason), *Ready to publish*, *Published (default)*, *Published (older)* or
*Withdrawn*.

- **Smoke test** — starts by itself when an import finishes, or with **Run smoke**. It boots a
  throwaway copy of the template in pool `pc-templates` on the provider's smoke network, with fresh
  cloud-init (a one-time SSH key nobody keeps, no password), waits for the guest agent, checks that
  cloud-init finished and the disk grew (and, for a Kubernetes node image, that kubelet and containerd
  are there), then deletes the copy. The provider needs a smoke network: set `config.templateSmoke`
  (for example `{"bridge":"vmbr0","vlan":29,"ip":"192.0.2.50/24","gateway":"192.0.2.1"}`, or just a
  bridge for DHCP) on the Proxmox provider. Without it the smoke test cannot run and the row says so.
- **Publish** — only after the template's latest smoke test passed. Confirm the digest. An OS template
  then joins the catalog, and the newest published version is what new servers get; older versions stay.
  A Kubernetes node image never appears in the OS catalog: publishing marks it ready and the row shows
  the exact version-map line to put on the Kubernetes provider.
- **Withdraw** — for a bad version: give a reason and confirm the digest. It leaves the catalog at once on
  every provider that has a copy (the previous published version takes over), its templates are kept for
  inspection, and its digest is blocked: it can never be imported or published again. If it is the only
  published version of that OS on a provider, you must tick *Withdraw anyway* and type the OS name, because
  no new server of that OS can then be created there until another version is published; if a node keeps
  no copy, you confirm that too. If a provider cannot be reached, its copy shows **Withdraw incomplete**:
  use **Finish withdraw** once it is back. For a Kubernetes node image the result gives the version-map
  line to put on the Kubernetes provider (it names images by VMID). Running servers are never affected.
- If the template is changed in Proxmox after its smoke test passed, run the smoke test again before
  publishing.

If an import or a smoke test fails, only what it created is removed, and the job shows why. To try again, start a
new import from this page: a failed import cannot be replayed from the durable jobs view (only one that
stopped mid-way, e.g. on an API restart, resumes on replay).

## Sandbox images

Sandbox image updates from the portal must be enabled for your site by the operator
(`PROXCLOUD_TEMPLATE_UPDATES_SANDBOX_ENABLED=true`); you need the *Manage external services* permission.

- **Update…** copies the newest published image, by digest, into the sandbox registry (the same digest,
  checked after the copy) and points the pre-pull on every node at it. Paste the exact digest to confirm.
  Templates switch to it once every Ready node has pulled it; until then sandboxes keep starting from the
  current image, which is never deleted. The row shows the progress: *Mirroring*, *Verifying digest*,
  *Updating prepull*, *Waiting for prepull on N/M nodes*, then *Switched*.
- **Rollback…** points the pre-pull back at the previous image; templates switch back once every Ready
  node has it.
- Each update is recorded as a pin that `make upgrade` reads, so an upgrade never puts an older image
  back. Use **Copy the images.yaml line to commit** so the repository matches. **Delete pin** forgets
  the portal's pin: the next `make upgrade` uses `deploy/templates/images.yaml` for that image again
  (nothing on the cluster changes).

When portal updates are off, use **Copy images.yaml pin**, put the line into
`deploy/templates/images.yaml` and run `make upgrade`: the installer copies the image into the sandbox
registry and pre-pulls it on every node, and the templates switch to it once every node has it.
