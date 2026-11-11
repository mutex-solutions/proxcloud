# 5. A self-contained Helm chart with optional bundled backing services, released to ghcr

## Status

Accepted design direction; deployment defaults and release details below are
historical and partially superseded.

## Current implementation note (2026-11-10)

Keep the original decision below as history, not an installation recipe. Current
[chart defaults](../../deploy/charts/proxcloud/README.md) use operator-managed
PostgreSQL with no RabbitMQ dependency; the CNPG operator is a prerequisite unless
explicitly bundled. Keycloak/external OIDC and dev subchart alternatives require
intentional configuration and real secrets.

The current workflows publish **five** images as `dev-<short-sha>` on
image-relevant `main` pushes, and publish the app chart only on a `v*` tag in a
separate workflow. There is no automatic chart-version tag-on-merge or enforced
version-bump gate. No ProxCloud release/chart is published yet. See
[CI/release operations](../ci-release.md) before interpreting the historical
publication claims below.

## Context

ProxCloud is several deployables — the API, the customer UI, the operator admin
UI — plus the backing services they need: PostgreSQL, RabbitMQ, and an OIDC issuer
(Keycloak). We want two things that pull in opposite directions:

- a **turnkey install** — someone can `helm install` and get a working system,
  databases and IdP included, without wiring up infrastructure first;
- a **production install** — an operator points ProxCloud at their own managed
  PostgreSQL, message broker, and identity provider, and the chart does not force a
  bundled copy on them.

We also want the chart to be understandable and self-contained (its own templates
should not depend on an external library chart we don't control), and we want
both the chart and the container images to be **published on every change** so a
consumer can pull a versioned artifact instead of building from source.

## Decision

Ship a single **application** Helm chart, `deploy/charts/proxcloud`, that owns all of
its own templates (`templates/`, with a local `_helpers.tpl`). It renders the
API `Deployment`, the two UI `Deployment`s, their services/ingress/routes (the
ingress exposes the public API paths, including `/mcp`), the encryption secret,
and the Keycloak realm config.

**Backing services are bundled as optional subchart dependencies**
(`Chart.yaml`), each gated by a condition and each with an external alternative:

| Backing service | Bundled subchart (condition)         | Point at an external instance |
| --------------- | ------------------------------------ | ----------------------------- |
| PostgreSQL      | `postgresql` (`postgresql.enabled`)  | `externalPostgresql.*`        |
| RabbitMQ        | `rabbitmq` (`rabbitmq.enabled`)      | `externalRabbitmq.*`          |
| Keycloak        | `keycloakx` (`keycloakx.enabled`)     | `auth.*.issuer`               |

Default values enable the bundled subcharts, so a plain install is complete. To
use managed infrastructure, set `*.enabled: false` and fill in the matching
`external*` block. The subcharts come from several registries — Bitnami OCI
(`postgresql`, `rabbitmq`), codecentric (`keycloakx`, running the official
Keycloak image), and CloudNativePG (`cloudnative-pg`, bundled only when
`cnpg.installOperator=true`) — while the ProxCloud templates themselves depend on
nothing outside the chart.

**Release is automated to GitHub Container Registry (ghcr.io) and version-driven:**

- **Container images** — `.github/workflows/docker.yml` builds the three images
  (`proxcloud-api`, `proxcloud-web`, `proxcloud-admin`) with Buildx and pushes them to
  `ghcr.io/<owner>/proxcloud-{api,web,admin}`. A push to `main` tags `dev`; a pushed
  git tag (`v*`) publishes under that tag name; a pull request builds only, no
  push. Auth is the built-in `GITHUB_TOKEN` (`packages: write`) — no external
  registry secrets.
- **Chart** — `.github/workflows/helm.yml` packages `deploy/charts/proxcloud` and
  pushes it to `oci://ghcr.io/<owner>/charts`. The chart `version` in
  `Chart.yaml` drives it: a PR that changes chart source must bump the version
  (CI fails otherwise), and a push to `main` publishes only if that version has
  no `proxcloud-<version>` git tag yet, then creates the tag. Re-running the same
  commit publishes nothing — the release is **idempotent**. The chart version is
  independent of the app image tags.

Deploy with `helm upgrade --install proxcloud oci://ghcr.io/<owner>/charts/proxcloud`
(or against the local chart directory during development).

## Consequences

- One `helm install` yields a working end-to-end system for evaluation and dev,
  with no external infrastructure prerequisites.
- The same chart serves production: flip the `*.enabled` flags off and supply
  `external*` connection details; nothing else changes.
- Images and chart are published automatically on merge/tag, so consumers pull a
  versioned artifact from ghcr instead of building locally; the built-in
  `GITHUB_TOKEN` keeps the pipeline free of external registry credentials.
- The chart-version gate makes a chart change un-mergeable without a version
  bump, and makes publishing idempotent, at the cost of contributors having to
  remember to bump `Chart.yaml`.
- Bundling databases and an IdP makes the chart heavier and gives it a wider
  surface to keep current (subchart version bumps, their transitive
  dependencies). Bundled instances are single-replica conveniences by default,
  **not** a production data tier — production should use the `external*` path.
- OCI distribution (images and chart) means consumers need an OCI-capable Helm
  (3.8+) and registry access to ghcr.
