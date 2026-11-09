# ProxCloud C4 model

The [C4 model](https://c4model.com) describes a software system at four zoom
levels: **Context** (the system, the people who use it and the systems it talks
to), **Containers** (the separately deployable or runnable pieces: processes,
databases, SPAs, charts), **Components** (the major building blocks inside one
container) and **Code** (the types and functions behind the few components where
the details matter). Each level zooms into one box of the level above. This
folder keeps one file per level, plus a change log that records *why* the
diagrams changed.

| File | Level | Answers |
|---|---|---|
| [c1-context.md](c1-context.md) | C1: System context | Who uses ProxCloud, and which external systems does it depend on? |
| [c2-containers.md](c2-containers.md) | C2: Containers | What runs where: management cluster, Proxmox cluster, storage tiers, build and delivery? |
| [c3-components.md](c3-components.md) | C3: Components | Which Go packages make up `proxcloud-api`, and what is each one responsible for? |
| [c4-code.md](c4-code.md) | C4: Code | How do the critical paths work: tenant network isolation, VM firewall and public IPs, quota, billing? |
| [project-invite-outbox.md](project-invite-outbox.md) | C3/C4: Invitation mail | How the default-OFF typed invitation producer atomically seals mail and cancels persisted rows without SMTP. |
| [Validation approval outbox](../../c4-validation-approved-outbox.md) | C1–C4: Approval-only mail | How the actual validation decision, immutable generation and keyless revocation share a commit, while activation remains outside SQL and delivery remains OFF. |
| [Savings contract-expired outbox](../../savings-expired-outbox.md) | C1–C4: Expired-term mail | How one canonical-term decision, immutable history and all four keyless cancellation paths share owned SQL boundaries while producer and delivery remain default OFF. |
| [Dormant snapshot registry core](../../registry-snapshot-core.md) | C1–C4: Snapshot prerequisite | How fixed snapshot envelopes, exact pin-role epochs and typed publication handoff stay dormant without capture callers, DEST retirement or GC authorization. |
| [Saved-runtime admission](../../saved-runtime-admission.md) | C1–C4: Fail-closed lifecycle | Why managed pause/resume/fork is held before effects while capture/pinned CREATE and explicit discard remain separate. |
| [CHANGELOG.md](CHANGELOG.md) | History | What changed in the architecture, in which PR, and why. |

The prose deep dives in [`docs/`](../../README.md) (auth, billing, jobs, data
model) go further on single subsystems. The C4 files are the map; where the two
disagree, the C4 status column describes the code on the default branch.

## Status legend

Every diagram element and every table row carries one of two statuses.

| Status | Meaning | How it is drawn |
|---|---|---|
| **Implemented** | Exists in the code on the default branch today. | Solid border, green fill, solid arrows. |
| **Planned** | On the roadmap, not in the code yet. Shown so readers see where the design is heading. | Dashed border, grey fill, dotted arrows. |

Removed elements are not drawn. Notable removals are recorded in
[CHANGELOG.md](CHANGELOG.md).

## Maintenance rule

The diagrams are only useful while they match the code, so they change in the
same PR as the code.

1. **Any PR that adds, removes or changes** a container, a component, an external
   system, a trust boundary (who authenticates to what, with which credential, or
   which network can reach which) or a key flow (the sequences in
   [c4-code.md](c4-code.md)) **must, in the same PR:**
   - update the affected diagram(s) and table rows, including the status column
     (a Planned element that ships becomes Implemented), and
   - append an entry to [CHANGELOG.md](CHANGELOG.md) with the date, the PR number,
     what changed, **why** it changed, and which files were touched.
2. **PRs that do not affect the architecture** (bug fixes inside a component,
   UI-only changes, tests, docs, dependency bumps) tick *No architecture impact*
   in the [PR template](../../../.github/pull_request_template.md) and give a
   one-line reason.
3. **Reviewers** check the box against the diff. A PR that moves a package, adds
   an external call or changes a flow without touching this folder should be sent
   back.
4. **File and line references** in [c3-components.md](c3-components.md) and
   [c4-code.md](c4-code.md) are a snapshot. Refresh the ones your PR moves; a
   function name is the stable reference, the line number is a hint.

## Changelog entry format

Newest entries go on top of [CHANGELOG.md](CHANGELOG.md).

```markdown
## YYYY-MM-DD: PR #NN <short title>

**What changed**
- <element>: Planned -> Implemented / new / removed / behaviour change

**Why**
- <the problem or decision that forced the change>

**Diagrams updated:** c2-containers.md (C2b), c4-code.md (sequence b)
```

## Rendering and conventions

The diagrams are [Mermaid](https://mermaid.js.org) code blocks. GitHub renders
them in the file view and in PR diffs; locally, the VS Code Markdown preview
(with a Mermaid extension) or [mermaid.live](https://mermaid.live) work.

Conventions that keep the diagrams rendering on GitHub:

- Use `flowchart` and `sequenceDiagram` only (not the experimental C4 syntax).
- Quote every node label (`id["label"]`); labels with `( ) : / , #` break otherwise.
- Never use `end` as a node id. Avoid `;` and `#` inside sequence messages.
- Keep a diagram to about 25 nodes. Split it when it grows past that.
- Mark status with the two shared class definitions, copied verbatim:

  ```text
  classDef impl fill:#e6f4ea,stroke:#1e7e34,color:#0b3d1a
  classDef planned fill:#f2f2f2,stroke:#8a8a8a,stroke-dasharray:5 5,color:#555555
  ```

  and draw interactions with Planned elements as dotted arrows (`-.->`).
