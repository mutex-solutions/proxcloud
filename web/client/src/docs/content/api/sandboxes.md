# Sandbox API

This reference covers the console's **session-authenticated project template API**, under the configured ProxCloud API prefix (normally `/api/v1`). The [Sandboxes guide](/docs/guides/sandboxes) describes the controls; [E2B API](/docs/api/e2b) describes external SDK/CLI use.

Every route below, **including GET**, requires a signed-in session and project cloud-resource management permission (`ProjectCloudResourceManage`). The project in the URL is authoritative; headers, body fields and template IDs do not select a different project. No browser-side API-key minting or storage is required.

```text
/project/{pid}/sandbox-templates
```

Responses use the normal ProxCloud data envelope. References below describe its `data` value. Errors do not include registry credentials.

## List and capabilities

- **GET** the root lists this project's safe template and named-snapshot records: ID, primary name, aliases, kind/mode, runtime CPU/memory/GPU, readiness and build history. Internal pause/fork records and other projects are not returned.
- **GET `/capabilities`** returns:

```json
{
  "importEnabled": true,
  "trustedWorkerAvailable": true,
  "warmingEnabled": true,
  "snapshotCaptureEnabled": true,
  "savedLifecycleEnabled": false,
  "savedLifecycleReason": "Managed snapshot resume/fork is held at this location; retained state is unchanged. Contact the platform operator.",
  "runtime": { "cpu": 2, "memoryGiB": 2, "maxCpu": 8, "maxMemoryGiB": 28 },
  "build": { "cpu": 4, "memoryGiB": 16, "diskGiB": 80,
    "maxCpu": 12, "maxMemoryGiB": 48, "maxDiskGiB": 160, "maxConcurrent": 2 }
}
```

Values are examples, not fixed platform limits. The console uses location defaults/limits and disables import or new builds when capabilities are unavailable. An enabled button does not bypass backend admission or reserve capacity.

## Import a digest

**POST `/import`** accepts:

```json
{
  "source": "registry.example.com/approved/agent@sha256:<64 lowercase hexadecimal characters>",
  "name": "my-agent",
  "cpuCount": 2,
  "memoryMB": 4096,
  "user": "app",
  "workdir": "/workspace",
  "startCmd": "python app.py",
  "readyCmd": "test -f /tmp/ready"
}
```

`user`, `workdir`, `startCmd` and `readyCmd` are optional. A private source may supply `registry: {username, password}` when allowed by the operator. Runtime memory is expressed in MiB (`memoryMB`); the console uses whole GiB and sends `GiB × 1024`.

The reference must be a supported immutable manifest digest from an operator-approved origin and exact repository; the example is not permission to import that host. The backend can substitute `{project}` in configured repository policies and defaults to disallowing anonymous import. The UI validates only generic immutable OCI syntax, never a hardcoded trust namespace.

An accepted request returns **202** with `{templateID, buildID, status, aliases?, reused?}`. Poll the project's template list/build logs. Import may finish after its dialog is closed; the frontend invalidates the **captured project's** list without changing a newer dialog. Do not reuse the immutable primary name to change runtime settings.

Registry credentials are ephemeral request data. They must not appear in DTOs, upload/fetch tokens, provenance/internal worker identifiers, logs, errors, notifications or browser/query/mutation caches.

## Aliases

**POST** or **DELETE `/{templateID}/aliases`** takes `{"aliases":["my-agent-stable"]}` and returns `{aliases:[primaryName,...extraNames]}`. The console changes one alias at a time with confirmation. Names have 1–63 lowercase letters, digits, dots, underscores or hyphens, beginning with a letter or digit. The canonical name is immutable; up to 19 additional names are allowed and must be unique within the project, without colliding with built-ins. Aliases are not tags.

## Prepare an image

**GET** or **POST `/{templateID}/prepare`** takes **no request body** and returns `{enabled,image,pool,pulled,total,ready,expiresAt?}`. POST requests temporary pre-pull intent; GET observes coverage. There must be a ready artifact and warming must be enabled by the location. Coverage is point-in-time and best-effort, never an SLA; `0/0` or expired/missing intent is not readiness.

## Build a new template

**POST `/{baseTemplateID}/builds`** accepts:

```json
{
  "name": "my-agent-with-tools",
  "cpuCount": 1,
  "memoryMB": 2048,
  "steps": [{ "type": "RUN", "args": ["python -m pip install numpy"] }],
  "buildCpu": 4,
  "buildMemoryGiB": 16,
  "buildDiskGiB": 80
}
```

`startCmd` and `readyCmd` are optional. A RUN step's optional second argument names its user (`args:[command,user]`); the console's form uses the default RUN user. It allows at most 64 steps; the backend accepts at most 100. There is no console COPY/ENV/upload or arbitrary raw-base editor.

The server resolves this project's ready base artifact and creates a **new template** with a distinct primary name. Runtime size and builder CPU/memory/disk are independent. It returns **202** `{templateID,buildID,status:"building"}`. Closing the dialog does not cancel accepted work.

## Build diagnostics and logs

**GET `/{templateID}/builds/{buildID}/logs?logsOffset=0`** returns `status`, optional `reason` and `step`, `logs`, optional `buildResources`, `diagnostics` and `nextLogsOffset`. The offset must be non-negative. Use the returned cursor for incremental API consumers; the console refreshes the bounded retained output from offset zero.

Safe diagnostics include phase state/reason/message, resource budgets, exit code/signal, warnings, archive timestamps, truncation, unavailable-output markers and a final failure tail. DTOs must not expose upload/fetch credentials, import credentials, internal worker IDs or provenance. Output is plain React text with ANSI/OSC controls stripped, not HTML.

The expanded panel polls active work and unfinished final-log capture; collapsing it stops polling. Older records and imported images can have no detailed archive. A failed latest build does not erase an older ready artifact.

## Delete and conflicts

**DELETE `/{templateID}`** removes the template/snapshot, aliases and build history. Running sandboxes are not killed; future starts can no longer resolve the removed template. If **any** build worker is still active, deletion returns **409** and is retryable after it finishes. Alias changes and new builds are also busy-gated.

There is **no build-cancel endpoint**. The Cancel buttons close dialogs only, and deletion is not a substitute cancellation operation.

## Existing runtime cloud API

Managed saved lifecycle is currently held: pause/resume/paused CONNECT/wake/fork and AutoPause/AutoResume promises return a fixed 403 before lifecycle effects. The native paused list exposes `resumeAvailable:false` and a fixed `resumeUnavailableReason`, never raw saved CRs or tokens. Explicit discard retains its existing active-lease/uncertainty fences; refusal does not rearm the original source deadline. Capture/export remains separate (`snapshotCaptureEnabled`).

Runtime sandboxes remain ordinary `SANDBOX` cloud resources under `/project/{pid}`. Existing resource listing, creation, cloud deletion and action requests (`EXEC`, `GET_LOGS`, `SET_TIMEOUT`) retain their own location, E2B, GPU and egress guards. Template controls do not replace or relax those gates. Use your deployment's API reference for the wider cloud-resource API and the [E2B API](/docs/api/e2b) for SDK access tokens and lifecycle operations.
