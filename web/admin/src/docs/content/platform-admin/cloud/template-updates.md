# Template updates

ProxCloud publishes its VM templates (Ubuntu, Debian and the Kubernetes node image) and its sandbox
images on GitHub Packages. **System > Template updates** shows the newest published version of each,
next to what your site runs, and lets you import a newer VM template.

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
- **Sandbox templates** — the newest image of each built-in sandbox template and the digest each
  sandbox provider's templates use.
- **Jobs** — every import you started, its progress and result.

Results are cached for two minutes; **Check now** asks GitHub again. The remaining GitHub API quota
is shown at the bottom.

## Importing a VM template

Imports must be enabled for your site by the operator (`PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED=true`);
until then the button is disabled. You need the *Manage external services* permission.

1. Choose **Import…** on a row.
2. Pick the Proxmox provider, the node, a storage with **import** content (where the download is
   placed) and the storage the template disk goes to. Only storages that are active on that node are offered.
3. For an OS template, choose whether to publish it to the catalog right away (off by default). Otherwise
   it is created *staged* (tag `pc-tu-staged`): boot a clone of it to check it, then add the `pc-public`
   tag in Proxmox to publish it. An import is not boot-tested before publishing, unlike a pipeline build,
   and a published import counts as a version when the pipeline prunes old templates on that node.
4. Paste the exact digest shown in the dialog to confirm, then **Import as new template**.

The download is checked three times (its SHA-256 while streaming, by Proxmox on upload, and against the
published digest), then imported as a **new** template VM in pool `pc-templates`, named
`pc-ghcr-<os>-…`. Existing templates are never changed or removed: remove old ones yourself when no
longer needed. A Kubernetes node image never appears in the OS catalog: put the new template's VMID into
the Kubernetes provider's version map.

If an import fails, only what that import created is removed, and the job shows why.

## Sandbox images

Updating sandbox images from the portal is not available yet. Use **Copy images.yaml pin**, put the line
into `deploy/templates/images.yaml` and run `make upgrade`: the installer copies the image into the
sandbox registry and pre-pulls it on every node, and the templates switch to it once every node has it.
