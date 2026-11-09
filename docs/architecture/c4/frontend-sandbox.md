# C4 component view: sandbox template console

## Scope and motivation

The ProxCloud client gains the recent sandbox template console capabilities without replacing its existing runtime sandbox page. Import, alias management, best-effort preparation, RUN-only builds and durable failure diagnostics use the signed-in session; the browser never needs to mint or store an E2B project key. Runtime E2B visibility, GET_LOGS actions, egress and GPU guards remain owned by the existing runtime page and backend.

## Components and boundaries

| Component | Responsibility |
|---|---|
| `SandboxesPage` | Existing runtime UI; keys `TemplatesTab` by project to reset template dialog state on navigation. |
| `SandboxTemplates` | Lists project templates, fetches location capabilities, polls any active worker and coordinates one action dialog. |
| `SandboxTemplateImport` | Validates generic immutable reference shape, holds optional registry credentials only in form memory, sends a direct async request and maps failures to credential-free messages. |
| `SandboxTemplateAliases` | Confirms individual alternate-name changes; preserves the immutable primary name and distinguishes aliases from build tags. |
| `SandboxTemplatePrepare` | Requests/observes temporary pre-pull intent and explicitly separates fresh, expired, zero-node and unavailable coverage. |
| `SandboxTemplateBuildDialog` | Creates a new template from a ready project base with ordered RUN steps and independent runtime/build budgets. |
| `SandboxTemplateBuildsDialog` / `SandboxTemplateBuildDiagnostics` | Expose build history, phase/exit/resource diagnostics, truncation/unavailable-output warnings and bounded plain-text logs. |
| `sandbox-template-utils` | Bounded input checks, URI-encoded project/template/build path construction and terminal-control stripping. |
| Project API | Session + ProjectCloudResourceManage gate on all console routes, authoritative URL project resolution, operator source policy, safe DTOs, worker admission and lifecycle. |

## Data flow

1. A project-scoped query loads the template list; a separate query loads runtime/build limits and import/warming availability. Capability failure disables operations that need those limits without hiding existing history.
2. A dialog is bound to the selected project and template. Reads and writes use the URL project, not body/header hints. Query keys include project and template/build identity.
3. Import uses `apiFetch` directly rather than React Query mutation variables. Username/password never enter query/mutation caches. Only an allowlisted success DTO or a generic status-derived failure reaches UI state. Form teardown clears credentials.
4. An accepted import invalidates the originally captured project's list even if its dialog closes or navigation changes project. Mounted checks prevent old completions from closing or notifying over a newer dialog. Builds also invalidate their captured project without changing a new dialog after teardown.
5. An expanded diagnostics component polls active work and unfinished archive capture. Unmount on collapse/project switch stops that observer. Logs are plain React text with ANSI/OSC/control sequences stripped; no HTML execution surface exists.
6. Prepare reports N/M image coverage with intent expiry. It is observational, not a reservation, readiness for 0/0 nodes, or a startup SLA.

## Security and lifecycle decisions

- The client checks immutable `repo@sha256` syntax only. The backend enforces exact operator-approved origins/repositories and project substitution; no source-brand host or namespace is trusted by UI code.
- Safe DTOs exclude import credentials, upload/fetch tokens, provenance and internal worker identifiers. Commands/output can be retained, so forms warn against embedded secrets.
- Any active build makes alias/new-build controls busy, including an older worker when the latest build failed. Backend 409 on deletion leaves a retryable confirmation; an older successful artifact can remain usable.
- Cancel/close is local dialog teardown. There is no worker-cancellation route or Delete-as-cancel behavior.
- New builds require a distinct primary name. The form accepts at most 64 RUN steps, no COPY/ENV or raw base editor. Runtime CPU/memory and builder CPU/memory/disk remain separate.
- Owned primary controls use high-contrast black text on brand orange or outline treatments. The scoped accessibility specs retain contrast checks, keyboard/Escape focus checks, and both 390px and desktop layouts in light/dark themes.

## Verification boundary

Stateful mock records are owned by a single project, with same-name/distinct-ID seeds for isolation tests. Mock source approval is an exact illustrative repository allow-list and does not configure production policy. Mocks reject cross-project IDs and active-worker deletion rather than reporting fake success.

The owned E2E specifications cover import/credential teardown, late completion, aliases, preparation, independent build budgets, diagnostics, malicious scope hints, busy conflicts, keyboard focus, theme/viewport accessibility and document routes. Test listing, static unit/contract checks, TypeScript and Vite build do not prove a live backend/browser integration. The parent integration lane serializes the full CI/browser run after API and core changes are merged; no live cloud, image or registry operation is part of this frontend work.
