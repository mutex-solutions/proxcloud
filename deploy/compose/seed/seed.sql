-- Same advisory lock as the api's own table creation (internal/pgdoc), so the two never race.
BEGIN;
SELECT pg_advisory_xact_lock(hashtext('pgdoc-create:billingConfiguration'));
CREATE TABLE IF NOT EXISTS "billingConfiguration" (id text PRIMARY KEY, doc jsonb NOT NULL);
INSERT INTO "billingConfiguration" (id, doc)
  SELECT 'seed', $seed${"baseCurrency":"USD","defaultConfiguration":true,"promotionCodesEnabled":true}$seed$::jsonb
  WHERE NOT EXISTS (SELECT 1 FROM "billingConfiguration");
COMMIT;
-- Same advisory lock as the api's own table creation (internal/pgdoc), so the two never race.
BEGIN;
SELECT pg_advisory_xact_lock(hashtext('pgdoc-create:platformConfiguration'));
CREATE TABLE IF NOT EXISTS "platformConfiguration" (id text PRIMARY KEY, doc jsonb NOT NULL);
INSERT INTO "platformConfiguration" (id, doc)
  SELECT 'seed', $seed${"branding":{"color":"#0066cc","name":"ProxCloud"},"dateConfiguration":{"dateFormat":"DD/MM/YYYY"},"defaultConfiguration":true,"language":"en","name":"ProxCloud","regions":[]}$seed$::jsonb
  WHERE NOT EXISTS (SELECT 1 FROM "platformConfiguration");
COMMIT;
