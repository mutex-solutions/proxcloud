-- Stop the barbican-api "secrets:get is disallowed by policy" log spam.
--
-- The v0.2.2+ sync gate skips a service type when it is toggled OFF for the
-- provider+region (config.services[slug][region] = false). But catalog
-- discovery auto-ENABLES every service it finds — including key-manager
-- (Barbican) — so the secret sync runs by default and the (correctly
-- restricted) sync token trips Barbican's secrets:get policy every cycle.
--
-- This disables key-manager for every region on every provider that has it, so
-- the sync no longer issues the Barbican list call. Idempotent; no-op when the
-- provider has no key-manager service. (Clients keep every other service; only
-- the rarely-used secrets UI is turned off. To OFFER Barbican instead, grant the
-- sync user secrets:get in the cloud's barbican policy rather than running this.)
--
-- Apply (operator):
--   Get-Content deploy\seed\disable-barbican-sync.sql -Raw |
--     kubectl --context <ctx> -n <ns> exec -i deploy/proxcloud-api -- sh -c 'psql "$PROXCLOUD_DB_URL"'

UPDATE "externalService"
SET doc = jsonb_set(
      doc, '{config,services,key-manager}',
      coalesce(
        (SELECT jsonb_object_agg(k, 'false'::jsonb)
           FROM jsonb_object_keys(doc #> '{config,services,key-manager}') AS k),
        '{}'::jsonb))
WHERE doc #> '{config,services,key-manager}' IS NOT NULL;

-- Read-back: every key-manager region should now read false.
SELECT id, doc->>'name' AS provider, doc #> '{config,services,key-manager}' AS key_manager
FROM "externalService"
WHERE doc #> '{config,services,key-manager}' IS NOT NULL;
