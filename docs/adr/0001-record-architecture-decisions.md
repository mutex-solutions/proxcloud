# 1. Record architecture decisions

## Status

Accepted

## Context

ProxCloud is a self-service platform for selling and operating OpenStack cloud
capacity: it fronts an OpenStack region with a customer console, a billing and
rating engine, and an operator admin surface. Decisions of this size — the
storage engine, the auth model, how background work is coordinated across pods —
are load-bearing and hard to reverse. New contributors need to understand not
just *what* the code does but *why* it is shaped that way, and we want that
rationale to live in the repository next to the code it explains rather than in
chat logs or someone's memory.

We want a lightweight, low-ceremony way to capture these decisions as they are
made, keep them under version control, and let later decisions supersede earlier
ones without rewriting history.

## Decision

We will keep Architecture Decision Records (ADRs) using Michael Nygard's format:
a short Markdown file per decision with **Title**, **Status**, **Context**,
**Decision**, and **Consequences** sections. Files live in `docs/adr/`, numbered
sequentially (`NNNN-title-with-dashes.md`).

- Status is one of `Proposed`, `Accepted`, `Superseded by ADR-XXXX`, or
  `Deprecated`.
- A decision that changes a previous one adds a new ADR and marks the old one
  superseded; we do not edit accepted records except to update their status.
- ADRs describe *decisions*, not tutorials — the operational and reference
  guides in `docs/` cover how to use the resulting system.

## Consequences

- The reasoning behind the big structural choices is discoverable in one place
  and reviewed like any other code change.
- There is a small, deliberate overhead: a non-trivial architectural change is
  expected to come with an ADR.
- The ADR log is append-mostly. Reading it top-to-bottom is a reasonable way to
  onboard onto the system's shape and its trade-offs.
