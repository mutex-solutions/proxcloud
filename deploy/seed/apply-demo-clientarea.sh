#!/usr/bin/env bash
# Demo seed for the admin "Client Area" pages (Users / Organizations / Projects /
# Billing profiles / Customer bills / Cloud resources). A fresh greenfield ProxCloud
# deploy has NO customer data, so those admin lists render empty — this upserts a
# small, LINKED demo dataset so the pages show rows.
#
# Idempotent: every row is an INSERT … ON CONFLICT (id) DO UPDATE keyed by a fixed
# hex id, so re-running is safe and never duplicates. The PostgreSQL DSN is read
# from the api pod env (PROXCLOUD_DB_URL) and psql runs from that pod (the image
# ships the client), so it works on any namespace without hardcoding.
#
# Usage:
#   deploy/seed/apply-demo-clientarea.sh <namespace> [api-pod] [kube-context]
#   deploy/seed/apply-demo-clientarea.sh proxcloud proxcloud-api-0
#
# Remove the demo data again:
#   deploy/seed/apply-demo-clientarea.sh <namespace> [api-pod] [kube-context] --clean
set -euo pipefail
NS="${1:?namespace required}"
API="${2:-proxcloud-api-0}"
KCTX="${3:-$(kubectl config current-context)}"
MODE="${4:-seed}"

# The api pod already holds the resolved PostgreSQL DSN; run psql from there.
DSN="$(kubectl --context "$KCTX" -n "$NS" exec "$API" -c api -- printenv PROXCLOUD_DB_URL | tr -d '\r')"
psql_pod() { kubectl --context "$KCTX" -n "$NS" exec -i "$API" -c api -- psql "$DSN" -v ON_ERROR_STOP=1; }

# Fixed ids: plain 24-char hex text in the `id` column.
# Cross-table references are stored as the hex STRING inside `doc`, the app's convention.
U1=000000000000000000000a01; U2=000000000000000000000a02
O1=000000000000000000000b01; O2=000000000000000000000b02
M1=000000000000000000000ba1; M2=000000000000000000000ba2
BP1=000000000000000000000c01
P1=000000000000000000000d01; P2=000000000000000000000d02
B1=000000000000000000000e01
R1=000000000000000000000f01; R2=000000000000000000000f02; R3=000000000000000000000f03

if [ "$MODE" = "--clean" ]; then
  psql_pod <<SQL
DELETE FROM "users" WHERE id IN ('$U1','$U2');
DELETE FROM "organization" WHERE id IN ('$O1','$O2');
DELETE FROM "organization_members" WHERE id IN ('$M1','$M2');
DELETE FROM "billingProfile" WHERE id = '$BP1';
DELETE FROM "project" WHERE id IN ('$P1','$P2');
DELETE FROM "bill" WHERE id = '$B1';
DELETE FROM "cloudResource" WHERE id IN ('$R1','$R2','$R3');
SQL
  echo "demo client-area data removed."
  exit 0
fi

NOW="$(date -u +%Y-%m-%dT%H:%M:%S.000Z)"

# up TABLE ID JSON — upsert one document (business fields live in `doc`; id in the id column).
up() {
  local table="$1" id="$2" doc="$3"
  psql_pod <<SQL
CREATE TABLE IF NOT EXISTS "${table}" (id text PRIMARY KEY, doc jsonb NOT NULL);
INSERT INTO "${table}" (id, doc) VALUES ('${id}', \$json\$${doc}\$json\$::jsonb)
  ON CONFLICT (id) DO UPDATE SET doc = EXCLUDED.doc;
SQL
}

# --- users ---
up users "$U1" '{"sub":"demo-user-01","modelVersion":1,"firstName":"Ada","lastName":"Lovelace","email":"ada@demo.example.com","consent":[],"customInfo":{},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'
up users "$U2" '{"sub":"demo-user-02","modelVersion":1,"firstName":"Alan","lastName":"Turing","email":"alan@demo.example.com","consent":[],"customInfo":{},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'

# --- organizations ---
up organization "$O1" '{"name":"Acme Cloud","description":"Demo organization","billingProfileId":"'"$BP1"'","customInfo":{},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'
up organization "$O2" '{"name":"Globex Labs","description":"Demo organization","customInfo":{},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'

# --- organization_members (separate table: {organizationId, sub, roles}) ---
up organization_members "$M1" '{"organizationId":"'"$O1"'","sub":"demo-user-01","roles":["OWNER"],"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'
up organization_members "$M2" '{"organizationId":"'"$O2"'","sub":"demo-user-02","roles":["OWNER"],"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'

# --- billing profile (ACTIVE, USD) ---
up billingProfile "$BP1" '{"organizationId":"'"$O1"'","status":"ACTIVE","email":"ada@demo.example.com","firstName":"Ada","lastName":"Lovelace","currency":"USD","company":true,"companyName":"Acme Cloud SRL","taxPayer":true,"country":"RO","city":"Cluj-Napoca","address":"1 Demo Street","zipCode":"400001","overwriteSuspension":false,"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'","activatedAt":"'"$NOW"'"}'

# --- projects (memberships embedded) ---
up project "$P1" '{"name":"acme-prod","status":"ENABLED","owner":"demo-user-01","organizationId":"'"$O1"'","billingProfileId":"'"$BP1"'","memberships":[{"sub":"demo-user-01","role":"OWNER"}],"services":[],"customInfo":{},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'
up project "$P2" '{"name":"globex-sandbox","status":"ENABLED","owner":"demo-user-02","organizationId":"'"$O2"'","memberships":[{"sub":"demo-user-02","role":"OWNER"}],"services":[],"customInfo":{},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'

# --- customer bill (SENT, USD, one line item; money as a decimal STRING) ---
up bill "$B1" '{"status":"SENT","invoiceCurrency":"USD","billingProfileId":"'"$BP1"'","billingCycle":{"startDate":"'"$NOW"'","endDate":"'"$NOW"'"},"items":[{"name":"Compute (acme-prod)","resourceType":"SERVER","currency":"USD","projectId":"'"$P1"'","netAmount":"42.50","appliedPricePlanRules":[],"createdAt":"'"$NOW"'"}],"sentAt":"'"$NOW"'","createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'

# --- cloud resources (under acme-prod) ---
up cloudResource "$R1" '{"projectId":"'"$P1"'","userId":"demo-user-01","type":"SERVER","region":"RegionOne","externalId":"demo-server-1","data":{"name":"web-01","status":"ACTIVE"},"info":{"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'
up cloudResource "$R2" '{"projectId":"'"$P1"'","userId":"demo-user-01","type":"VOLUME","region":"RegionOne","externalId":"demo-volume-1","data":{"name":"web-01-disk","size":50,"status":"in-use"},"info":{"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'
up cloudResource "$R3" '{"projectId":"'"$P1"'","userId":"demo-user-01","type":"NETWORK","region":"RegionOne","externalId":"demo-net-1","data":{"name":"acme-net","status":"ACTIVE"},"info":{"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"},"createdAt":"'"$NOW"'","updatedAt":"'"$NOW"'"}'

psql_pod <<'SQL'
SELECT 'users' AS table, count(*) FROM "users"
UNION ALL SELECT 'organization', count(*) FROM "organization"
UNION ALL SELECT 'organization_members', count(*) FROM "organization_members"
UNION ALL SELECT 'billingProfile', count(*) FROM "billingProfile"
UNION ALL SELECT 'project', count(*) FROM "project"
UNION ALL SELECT 'bill', count(*) FROM "bill"
UNION ALL SELECT 'cloudResource', count(*) FROM "cloudResource";
SQL
echo "demo client-area data seeded."
