# Sandboxes

**Platform → Sandboxes** provides short-lived, isolated environments for running code. Your operator chooses the available templates, GPU models, runtime limits and network policy. If sandboxes are not enabled for the project, ask the operator to attach a sandbox location.

A sandbox is not a persistent server: deleting it or reaching its time limit removes its files unless you have separately saved them through a supported snapshot or pause operation.

## Start and use a sandbox

Select **New sandbox**, choose an offered template, set runtime vCPU, memory and a time limit, and select **Start sandbox**. GPU templates retain their configured GPU; choose a CPU-only template for a CPU-only workload. The server checks current availability and project limits; an enabled control is not a reservation.

On a running sandbox, **Run command** shows output and the exit code. **Logs** retrieves recent output. The row menu also offers timeout extension and deletion. These controls keep the location's existing E2B, GPU and egress restrictions; changing template build settings does not change those restrictions.

## Project templates

The **Templates** tab appears where the location offers the E2B-compatible API. It lists this project's templates, imported digests and named snapshots. An older successful artifact can remain ready even when the latest build failed.

All template console operations use your signed-in session and require **project cloud-resource management permission**, including reads. You do **not** need to create or store a sandbox API key in the browser. Changing projects closes open template dialogs and clears unfinished forms and registry credentials.

### Import an image digest

1. Select **Import image digest**.
2. Enter a `repository@sha256:<64 lowercase hexadecimal characters>` from an **operator-approved registry** and choose a new template name. Use a linux/amd64 manifest digest, not a tag, URL or multi-platform index.
3. Set **Runtime vCPU** and **Runtime memory (GiB)** within the fetched location limits. These size future sandboxes, not the trusted import worker.
4. Optionally set a named user, an absolute working directory, a start command or a ready command. Blank settings inherit the image's supported defaults; numeric UID or `USER:GROUP` overrides are not supported here.
5. If the approved source requires your pull credential, enable **Private registry** and supply a username and password. The server decides whether that source and credential mode are permitted. Valid reference syntax does not authorize an origin or repository.
6. Select **Import image**. An accepted operation appears in the project's list while verification completes. A first import can transfer image layers; import does not run customer RUN steps or promise zero transfer.

The operator's source policy is an exact origin/repository allow-list and can substitute the current project in approved repository names. It is not permission to import any image under a registry host or namespace. Anonymous access is not enabled by default; ask the operator which credential mode is allowed.

Credentials exist only in the open form and the in-flight request. They are not saved in browser storage, React Query mutation variables, template records, notifications or logs. They are cleared on close, Cancel, success or project switch. Do not place secrets in image references or commands: commands and their output may be retained in history.

**Cancel** and the close button close the dialog; they do not stop an accepted import. If the response arrives later, the captured project's template list is refreshed even when you have closed the dialog or changed projects.

### Names, aliases and tags

Names contain 1–63 lowercase letters, digits, dots, underscores or hyphens, beginning with a letter or digit. Names and aliases must be unique within the project and must not collide with a built-in template name.

**Manage aliases** adds or removes an individual alternate name after confirmation. A template has one immutable primary name and up to 19 additional aliases. The primary name cannot be renamed or removed here. Aliases select a template; build tags select a version of its artifact. Removing an alias is not deleting a template or an artifact still in use. **Copy name** copies the primary SDK name.

### Prepare an image

**Prepare → Prepare image** requests a temporary image pre-pull on the eligible runtime pool when the operator enables warming. The dialog reports the image, pool, expiry and **Coverage: N/M nodes**. It refreshes while open and the intent remains active.

Coverage is a point-in-time observation, **not a startup latency guarantee or SLA**. Disabled preparation, stale or expired intent, no ready artifact, unavailable data and zero eligible nodes are shown explicitly. `0/0` is not ready coverage. Node replacement and image garbage collection can make the next start cold again.

### Build with independent budgets

From a ready project template, choose **New build**, enter a **new template name**, and add ordered **RUN command** steps. The base name and aliases cannot be reused. The server selects the ready base artifact; the console does not accept a raw base-image editor, COPY uploads or ENV steps.

Set runtime size separately from **Builder vCPU**, **Builder memory (GiB)** and **Builder disk (GiB)**. Defaults, maximums and concurrency limits come from the location. The disk budget covers compressed base data, the expanded filesystem and output together. The console supports up to 64 RUN steps; the backend contract permits at most 100. Use the SDK/CLI for advanced recipes supported by your location.

Closing or cancelling this dialog does not stop an accepted build. There is no console worker-cancellation endpoint or button.

### Inspect progress and failures

**Builds and logs** lists build tags, source/image, runtime startup commands and independent builder resources. Expand **Diagnostics and logs** for retained logs and available phase states, resource budgets, exit codes, signals, reasons and the final failure tail. Active work and unfinished final-archive capture refresh while expanded; collapsing the panel stops its polling.

Truncated or unavailable output is marked explicitly, not treated as an empty successful log. Output is rendered as plain text with terminal ANSI/OSC controls stripped; HTML-like output is not executed. Old records and imported images may have no detailed phase archive.

A template is busy while **any** recorded build is active, not just its newest build. Alias changes and new builds are then disabled. **Delete** removes the template/snapshot, aliases and build records; new sandboxes cannot start from it, while running sandboxes keep running. An active-worker conflict returns a retryable error and leaves the confirmation dialog open. Retry after the worker finishes; Delete is not a cancellation mechanism.

## E2B SDK and CLI

Use **API keys** only for external SDK/CLI clients. Copy the key once and use the exact E2B domain shown by your location. Keep credentials outside source control and respect the operator's VPN/network restrictions. The browser template controls do not need that key.

```bash
export E2B_DOMAIN=<domain shown on the API keys tab>
export E2B_API_KEY=<your project key>
```

```python
from e2b import Sandbox

sandbox = Sandbox.create("my-template")
print(sandbox.commands.run("echo hello").stdout)
sandbox.kill()
```

For compatibility and request details, see the [E2B API](/docs/api/e2b) and the session-authenticated [Sandbox API](/docs/api/sandboxes). Large cold images may require preparation and a longer SDK request timeout; runtime TTL is separate and starts when the sandbox is ready. Do not expose a private API or registry publicly to work around network access.

## Billing and limits

Runtime sandboxes are metered by runtime vCPU, memory and GPU while running. **Template builds use their independent builder resource budget for build usage**, not the future sandbox's runtime size. Stored snapshots and paused filesystem storage are separate from running compute. Actual prices, storage charging and duration rules are chosen by your operator's price plan; see [Metering and billing](/docs/concepts/billing-and-metering).

The location and project also enforce runtime size, time limit, concurrent workers, GPU availability and other quotas. The console displays fetched capabilities for guidance; backend admission remains authoritative.
