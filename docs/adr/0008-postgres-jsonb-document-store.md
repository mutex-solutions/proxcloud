# 8. PostgreSQL + JSONB as the document store

## Status

Accepted.

## Context

The domain is modelled as one document per aggregate — aggregates are irregular,
nested, per-tenant variable, and almost always read and written whole. The open
question is the engine that holds those documents. A dedicated document database
would serve, but a general-purpose relational engine avoids the costs it carries:

- **One fewer moving part.** Deployments already run PostgreSQL for Keycloak. A
  second, separate document database would double the stateful surface operators
  have to run, back up, and secure.
- **A cleaner licensing and operational story.** A single, ubiquitous,
  permissively-licensed relational engine is easier to bundle, self-host, and
  point at a managed instance than a second document database.
- **PostgreSQL is a competent document store.** `jsonb` gives schemaless
  storage, containment and path operators, and expression indexes — enough to
  serve the small query vocabulary the app actually uses — while adding real
  transactions, row locking, and joins where they help.

We wanted the move to change the *storage*, not the *domain*: keep the
document-per-aggregate shape, keep the serialized wire semantics the parity
tests pin, and avoid rewriting business logic.

## Decision

Store each aggregate kind in its own table shaped `(id text primary key, doc
jsonb not null)`, one table per former collection (same names). A thin in-repo
library, `internal/pgdoc`, is the only thing that talks to the database:

- **Documents** are plain JSON encoded from the domain structs' `json` tags
  (`encoding/json`): `time.Time` serializes as an RFC3339 string, money
  (`decimal.Decimal`) as a decimal string (`"12.34"`), and `omitempty` decides
  null-vs-absent. Postgres `jsonb` keeps the decimal string at full precision, so
  money is never a float; time-range filters cast `::timestamptz` and time
  indexes use an IMMUTABLE `pgdoc_ts()` wrapper.
- **Identifiers** are 24-char hex strings minted in the app (a time prefix, a
  per-process random part, and a counter, so they sort roughly by creation
  order). There is no separate binary id type and no id-vs-string
  dual-keying; ids are plain strings end to end.
- **Queries** use the small operator subset the code actually needs
  (`$eq/$ne/$in/$nin/$exists/$gt/$gte/$lt/$lte/$or/$and/$regex/$contains/
  $elemMatch`, dotted paths, `_id` → the `id` column), translated to JSONB
  predicates. Array-of-object membership is `jsonb @>` containment. Hot paths
  get expression indexes (`(doc->>'sub')`, `(doc->>'organizationId')`, …); the
  old unique constraints become unique expression indexes.
- **Atomic operations** that were single-document `findAndModify` become short
  transactions with `SELECT … FOR UPDATE`: the cloud-resource cache
  upsert/optimistic-concurrency update and the bill-locking claim. The
  distributed job lock (ShedLock) is a `shedLock` table claimed with
  `INSERT … ON CONFLICT (id) DO UPDATE … WHERE` the existing lock has expired.
- **The three former aggregation pipelines** are hand-written SQL: `GROUP BY`
  for the cloud resource type-counts, and a `LEFT JOIN` for the
  savings-contract ↔ billing-profile lookup.

`internal/pgdoc` deliberately implements only these primitives — it is not a
general-purpose document-query-to-SQL translator.

## Consequences

- New aggregate attributes and per-tenant variation still cost no schema change
  — they live inside `doc`.
- Deployments run one database engine. The bundled `postgresql` subchart hosts a
  dedicated `proxcloud` database; `externalPostgresql.*` points at a managed
  instance instead.
- Cross-aggregate reads that were pipelines are now SQL joins/aggregates, which
  PostgreSQL plans and indexes natively.
- Aggregate invariants are upheld in application logic plus unique **expression**
  indexes — with real transactions available where a multi-statement invariant
  benefits from one.
- The document wire format is pinned by the same parity/serialization tests, so
  the storage swap is invisible to API consumers.
- Config-as-documents is unchanged: a new environment still runs the seed step
  (`deploy/seed`); a schema-bootstrap/migration step provisions the tables.
