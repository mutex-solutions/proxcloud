#!/usr/bin/env bash
# Config-as-code seed: applies the versioned default config docs into a
# deployment's PostgreSQL, idempotently (upsert — never deletes). Greenfield
# deployments need these before billing/project endpoints work, and they power
# the platform-configuration / billing-configuration read endpoints + the base
# currency used by new billing profiles.
#
# Each document kind is a `(id text primary key, doc jsonb not null)` table.
# These two are singletons the app reads with an empty filter, so a single fixed
# `id` row per table is enough; the JSON file IS the stored `doc`.
#
# Usage:
#   deploy/seed/apply-seed.sh <namespace> [api-pod] [kube-context]
# Example:
#   deploy/seed/apply-seed.sh proxcloud proxcloud-api-0 
#
# NOTE: platformConfiguration.language MUST be a valid language constant
# ("en"/"ro"); billingConfiguration.baseCurrency feeds new profiles + the
# language getter (RON → RO, else EN).
set -euo pipefail
NS="${1:?namespace required}"
API="${2:-proxcloud-api-0}"
KCTX="${3:-$(kubectl config current-context)}"
DIR="$(cd "$(dirname "$0")" && pwd)"

# The API pod already holds the resolved PostgreSQL DSN; run psql from there
# (the image ships the client) against that same DSN.
DSN="$(kubectl --context "$KCTX" -n "$NS" exec "$API" -c api -- printenv PROXCLOUD_DB_URL | tr -d '\r')"

seed_one() { # table json-file
  local table="$1" file="$2" doc
  doc="$(tr -d '\n' < "$DIR/$file")"
  kubectl --context "$KCTX" -n "$NS" exec -i "$API" -c api -- \
    psql "$DSN" -v ON_ERROR_STOP=1 <<SQL
CREATE TABLE IF NOT EXISTS "${table}" (id text PRIMARY KEY, doc jsonb NOT NULL);
INSERT INTO "${table}" (id, doc) VALUES ('seed', \$json\$${doc}\$json\$::jsonb)
  ON CONFLICT (id) DO UPDATE SET doc = EXCLUDED.doc;
SELECT '${NS} ${table} count:' AS msg, count(*) FROM "${table}";
SQL
}

seed_one platformConfiguration platform-configuration.json
seed_one billingConfiguration billing-configuration.json
