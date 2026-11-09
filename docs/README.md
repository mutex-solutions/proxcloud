# ProxCloud documentation

Contributor and operator documentation for ProxCloud — a self-service platform for
selling, billing, and operating Proxmox VE private-cloud capacity (a Go backend, two React
19 SPAs, and a Helm chart).

> **Audience.** These docs are for people working *on* ProxCloud. End-user and
> operator product help ships inside the SPAs themselves at `/docs` — a different
> audience and a separate source.

## Getting started

- [quickstart.md](quickstart.md) — pick a path (laptop, one node, production, split, Helm),
  the keys to fill, the one command, first login and first-install fixes.
- [install.md](install.md) — the one-shot installer: `make install`, its stages, options
  and day-2 commands.
- [development.md](development.md) — prerequisites, build, run the full stack with
  `docker compose`, run the backend and SPAs locally, seed config, point at a dev
  cloud, and the edit/run loop.
- [configuration.md](configuration.md) — the full configuration contract:
  `application.yml` keys and the environment variables that override them.

## Architecture

- [architecture.md](architecture.md) — the big picture: the single API binary,
  the handler→service→repo layering, the SPAs, and how they fit together.
- [data-model.md](data-model.md) — the PostgreSQL/JSONB document model, the core
  aggregates and their tables, id and serialization conventions.
- [Architecture diagrams (C4)](architecture/c4/README.md) — system context,
  containers, components and code views with Implemented/Planned status, and the
  architecture change log.

## Subsystems

- [Validation approval outbox](validation-approved-outbox.md) — default-OFF approval-only
  mail decisions for the actual billing-profile validation document; atomic generation,
  keyless revocation/history, and explicitly separate postcommit activation effects.
  [Dedicated C4 view](c4-validation-approved-outbox.md).

- [mail-outbox-safety-limits.md](mail-outbox-safety-limits.md) — dormant primary mail
  infrastructure, frozen encrypted envelopes, typed SMTP receipts and UNKNOWN holds;
  no business activation or resend API.

- [auth.md](auth.md) — the OAuth2/OIDC resource server, realms per audience, and
  SigV4 keys for the public admin API.
- [billing.md](billing.md) — price plans and rules, rating and bills,
  transactions, account credit, savings, promotions, activation/suspension.
- [cloud-integration.md](cloud-integration.md) — Proxmox VE providers, the
  resource cache and sync, traffic metrics, the Ceph RGW S3 backend and the
  managed K8s/DBaaS providers.
- [managed-k8s.md](managed-k8s.md) and [managed-dbaas.md](managed-dbaas.md) —
  managed Kubernetes and database clusters delivered through ArgoCD.
- [managed-sandbox.md](managed-sandbox.md) — gVisor sandboxes on a platform-owned
  Kamaji cluster: isolation, release gates, the public entry and wake, the sandbox
  registry and custom templates, with what ran on the lab.
- [e2b-compat.md](e2b-compat.md) — the E2B-compatible sandbox API under `/e2b`.
- [saved-runtime-admission.md](saved-runtime-admission.md) — deliberate managed lifecycle
  admission hold, retained-copy/TTL boundaries, separate capture/pinned CREATE, truthful
  capabilities and the still-unimplemented full restore protocol.
- [pricing-rate-card.md](pricing-rate-card.md) — the seed price plan's rules and the
  benchmarks behind them.
- [proxmox-permissions.md](proxmox-permissions.md) — least-privilege PVE roles
  (ProxCloudPlatform, ProxCloudCAPI, ProxCloudCSI and the rest), ACL paths, privsep tokens
  and the `pveum` commands to create them.
- [jobs-scheduling.md](jobs-scheduling.md) — scheduled jobs, the PostgreSQL
  distributed lock, and the optional RabbitMQ charge fan-out.

## Reference

- [api.md](api.md) — the HTTP surfaces: the customer `/api/v1`, the operator
  admin routes, the public `/admin-api/v1`, and the `/mcp` endpoint.
- [glossary.md](glossary.md) — domain vocabulary (organization vs. project vs.
  billing profile, user vs. member vs. owner, pricing terms, and more).
- [testing.md](testing.md) — unit tests and the tag-gated integration suite
  (testcontainers PostgreSQL), what's covered, and how to add a test.

## Decisions

Architecture Decision Records ([adr/](adr/)) — the load-bearing choices and their
rationale, in Nygard format:

- [ADR-0001](adr/0001-record-architecture-decisions.md) — Record architecture
  decisions.
- [ADR-0002](adr/0002-go-and-layered-architecture.md) — Go with a
  handler→service→repository layering over the document store.
- [ADR-0003](adr/0003-auth-as-oauth2-resource-server.md) — Authenticate as an
  OAuth2/OIDC resource server (plus SigV4 for machines).
- [ADR-0005](adr/0005-self-contained-helm-chart.md) — A self-contained Helm chart
  with optional bundled backing services, released to ghcr.
- [ADR-0006](adr/0006-mcp-in-process-dispatch.md) — Expose an MCP server that
  dispatches in-process through the existing API.
- [ADR-0007](adr/0007-distributed-job-locking.md) — Run scheduled jobs under a
  distributed lock, with optional broker fan-out.
- [ADR-0008](adr/0008-postgres-jsonb-document-store.md) — PostgreSQL + JSONB as
  the document store (supersedes ADR-0004's engine choice).
