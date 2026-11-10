# Managed PostgreSQL patch baseline

## Scope and evidence

This is a **source-only DBaaS change**, based on commit
`8bcfc517078a7d7b9a5c8008f689b23e9579924d`. It updates same-major PostgreSQL
image targets and the catalog used when the installer creates a **new** managed
DBaaS provider. It is not an executed upgrade or a release announcement.

Targets follow official metadata collected on **2026-11-10 UTC**, including the
[PostgreSQL support policy](https://www.postgresql.org/support/versioning/) and
[PostgreSQL 18.6 release notes](https://www.postgresql.org/docs/release/18.6/).
The release notes establish patch targets, not versions actually running at a site.
Parent additionally queried the public official registry manifests without pulling
or running images. The four tags resolved to these index digests:

| Tag | Observed index digest |
| --- | --- |
| `15.19` | `sha256:11274300143f02d369ea2ff6082b028b6b662915f06bf59f26b808b53823475a` |
| `16.15` | `sha256:34cd4159d07b3410a1b29da35072e2e927a0905e717a61afa3814624a2e8859a` |
| `17.11` | `sha256:70664ebcfa1100361b5bdc28bbf06fdbe08db2dc4ad7bd14de33c5e05fe8ea8e` |
| `18.6` | `sha256:899d3ed526b659d77935dde0e6bf2d69dbbf17d3d8c6486ca8cfd04bd3c18533` |

These are dated registry availability/integrity observations, not digest-pinned
rendered values or startup/operator/extension/restore certification. The chart
intentionally retains tag-only image syntax in this bounded patch.

## Source changes

The [DBaaS image helper](<../deploy/charts/database-cluster/templates/_helpers.tpl>)
keeps its provider-facing major keys and resolves them as follows:

| Major key | Previous image tag | New image tag |
|---|---|---|
| `15` | `ghcr.io/cloudnative-pg/postgresql:15.18` | `ghcr.io/cloudnative-pg/postgresql:15.19` |
| `16` | `ghcr.io/cloudnative-pg/postgresql:16.14` | `ghcr.io/cloudnative-pg/postgresql:16.15` |
| `17` | `ghcr.io/cloudnative-pg/postgresql:17.10` | `ghcr.io/cloudnative-pg/postgresql:17.11` |
| `18` | `ghcr.io/cloudnative-pg/postgresql:18.4` | `ghcr.io/cloudnative-pg/postgresql:18.6` |

The [chart metadata](<../deploy/charts/database-cluster/Chart.yaml>) changes from
`0.5.0` to **`0.5.1`**. This is an unpublished source version: no chart has been
packaged, published, installed, or used to re-pin an existing provider here.

The new DBaaS provider entry in [stage 60 registration](<../installer/playbooks/60-register.yml>)
offers exactly PostgreSQL `17` and `18`, defaults to `18`, and retains replica
choices `[1, 3]`. The existing name-absence condition and POST-only loop are
unchanged. A provider already present under that name is skipped; this patch
adds no existing-provider update, catalog rewrite, chart re-pin, resource
rewrite, or data operation. The fresh entry still gets its chart version from
[the checkout-derived variable](<../installer/playbooks/group_vars/all/computed.yml#L35-L37>).
Publishing and verifying that chart is a separate release gate.

## Boundaries and residual risks

- **PostgreSQL 14 is deliberately unchanged.** Its image mapping remains
  `ghcr.io/cloudnative-pg/postgresql:14.23`; it has not been silently removed.
  The report identifies `14.24` as a later patch and **2026-11-12** as PostgreSQL
  14's end-of-support date. Retaining the old mapping leaves a patch/lifecycle
  residual for any provider or resource still using 14; it is not an assurance
  of support. Inventory and retirement/migration require a separate decision.
- Keys `15` and `16` remain available to existing configurations. Their patch
  map coverage does not add them to the installer's curated new-provider offer.
  No catalog belonging to an existing provider is trimmed or changed.
- The [dev seed](<../deploy/seed/external-service-dev.json#L86-L106>) remains an
  explicit PostgreSQL 17-default example with versions `16`, `17`, `18` and
  chart pin `0.5.0`. It is not an updated release catalog. The existing
  [PostgreSQL restore example](<../deploy/charts/database-cluster/examples/values-postgresql-restore.yaml#L3-L10>)
  also stays on major `17`; changing its tag to `18` would not establish
  cross-major physical backup compatibility.
- The portal datastore is separate. Its [CNPG values](<../deploy/charts/proxcloud/values.yaml#L343-L352>)
  still use the moving major tag `ghcr.io/cloudnative-pg/postgresql:18`.
  This patch does **not** establish that portal image's minor, build, digest,
  deployed version, or patch status. Optional portal CNPG/Keycloak dependencies
  and all non-PostgreSQL DBaaS images/operators are untouched.
- No SQL/major-upgrade feature or action code changes. PostgreSQL 17 to 18 is
  a separate data migration, not a fix for 17.10 to 17.11. The existing
  [numeric upgrade guard](<../internal/cloud/dbaas/engines.go#L180-L214>) remains
  unchanged and does not prove extension, locale, base OS, or operator
  compatibility. Physical backup/WAL recovery is not a cross-major migration
  mechanism; preserve separate recovery/archive planning for any later major
  upgrade.

## Regression test source and validation limits

[The native chart regression](<../internal/cloud/dbaas/postgresql_patch_test.go>)
passes major keys `15`, `16`, `17`, and `18` through `BuildValues` and the
existing [real Helm render helper](<../internal/cloud/dbaas/chart_test.go#L16-L57>).
It asserts the single CNPG Cluster's exact `spec.imageName` for each target and
that `engineVersion` remains the major key. It requires Helm when `CI` is set,
rather than silently skipping there; the existing local skip without Helm is
retained. This checks the render contract, not operator admission or database
startup.

[The installer regression](<../installer/tests/test_postgresql_catalog.py>)
reads repository YAML as data only. It checks the fresh provider's curated
offer/default, replica choices, unchanged name-absence condition, POST body,
chart-version derivation, and absence of a separate **literal** DBaaS update
payload in that playbook. It does not render Ansible/Jinja, import installer
code, contact an API, or access generated site files. These are structural
assertions, not a proof of all possible runtime control flow.

The initial implementation was prepared by source inspection only. Passing results
must be recorded separately by the Parent-controlled test lane; no live database
acceptance is implied. The [installer test target](<../installer/Makefile#L35>) now
includes this new data-only regression explicitly, alongside existing filter/ICMP
tests, so normal test execution cannot silently omit the catalog contract.

Before any separately approved release or rollout:

1. Validate the source tests with the approved tools; require actual Helm
   rendering in CI. Neither source inspection nor a skipped chart test is a
   passing render result.
2. Verify registry tag availability, supported platform/base OS and extensions,
   and resolve/review immutable digests. These patch tags are **not** digest
   pins; no artifact or runtime attestation is supplied here.
3. Package/publish the chart and verify fresh-provider registration separately.
   Review any change to an existing chart pin as its own operational change;
   applying the new map can request a same-major image rollout.
4. Before a data-affecting rollout, establish the deployed operator/CRD/plugin
   matrix, release-note actions, WAL archiving, full backup and successful
   restore/PITR evidence. No backup, recovery, failover, runtime compatibility,
   or data-preservation proof is provided by this patch.
