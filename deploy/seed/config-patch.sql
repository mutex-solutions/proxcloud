-- Config patch (idempotent). Fixes the price-rule name mojibake from the first
-- seed run (PowerShell re-encoded the em-dash to '?'), fills the billing
-- configuration with placeholder business details (edit before applying), and seeds a starter set
-- of image categories. ASCII-only so no pipe re-encoding can corrupt it.
--
-- Apply (operator):
--   Get-Content deploy\seed\config-patch.sql -Raw |
--     kubectl --context <ctx> -n <ns> exec -i deploy/proxcloud-api -- sh -c 'psql "$PROXCLOUD_DB_URL"'

-- 1. Repair the price-rule names (ASCII) --------------------------------------
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('Compute: CPU + RAM components'::text))                       WHERE id = '6a4e63e8aa5e5eed10000002';
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('GPU: nvidia-4090 (RTX 4090)'::text))                        WHERE id = '6a4e63e8aa5e5eed10000003';
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('GPU: nvidia-3090 (RTX 3090)'::text))                        WHERE id = '6a4e63e8aa5e5eed10000004';
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('GPU: nvidia-3080ti (RTX 3080 Ti)'::text))                   WHERE id = '6a4e63e8aa5e5eed10000005';
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('GPU: nvidia-a6000 (RTX A6000 Ampere 48GB)'::text))          WHERE id = '6a4e63e8aa5e5eed10000006';
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('GPU: nvidia-pro-6000 (RTX PRO 6000 Blackwell 96GB)'::text)) WHERE id = '6a4e63e8aa5e5eed10000007';
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('GPU: nvidia-pro-4500 (RTX PRO 4500 Blackwell 32GB)'::text)) WHERE id = '6a4e63e8aa5e5eed10000008';
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('GPU: intel-a60'::text))                                     WHERE id = '6a4e63e8aa5e5eed10000009';
UPDATE "pricePlanRule" SET doc = jsonb_set(doc, '{name}', to_jsonb('Public egress: 1 TiB free, then $0.01/GB'::text))           WHERE id = '6a4e63e8aa5e5eed10000010';

-- 2. Billing configuration: placeholder business details -------------------
-- Surgical merge: keeps every existing key (base currency, gateways, activation
-- flow, suspension), only adds/overwrites name + company.businessName + address.
UPDATE "billingConfiguration"
SET doc = doc
  || jsonb_build_object('name', 'Example Cloud')
  || jsonb_build_object('company', coalesce(doc->'company', '{}'::jsonb)
       || '{"businessName": "Example Cloud Ltd."}'::jsonb)
  || jsonb_build_object('address', coalesce(doc->'address', '{}'::jsonb)
       || '{"country": "US", "city": "Example City", "address": "1 Example Street"}'::jsonb)
WHERE doc->>'defaultConfiguration' = 'true';

-- 3. Image categories (top-level buckets; image groups hang under them) -------
-- Simple docs (name/description/bareMetal). Add image GROUPS under these in the
-- admin Catalog UI (they bind the provider's template/image names, which are cloud-specific).
CREATE TABLE IF NOT EXISTS "imageCategory" (id text PRIMARY KEY, doc jsonb NOT NULL);
INSERT INTO "imageCategory" (id, doc) VALUES
('6a4e63e8aa5e5eed10000020', '{"bareMetal": false, "name": "Ubuntu", "description": "Ubuntu LTS server images"}'),
('6a4e63e8aa5e5eed10000021', '{"bareMetal": false, "name": "Debian", "description": "Debian stable server images"}'),
('6a4e63e8aa5e5eed10000022', '{"bareMetal": false, "name": "Windows", "description": "Windows Server images"}'),
('6a4e63e8aa5e5eed10000023', '{"bareMetal": false, "name": "GPU / ML", "description": "GPU-ready images with CUDA / ML frameworks preinstalled"}')
ON CONFLICT (id) DO NOTHING;

-- Read-back
SELECT id, doc->>'name' AS rule_name FROM "pricePlanRule" WHERE doc->>'pricePlanId' = '6a4e63e8aa5e5eed10000001' ORDER BY id;
SELECT doc->>'name' AS cfg_name, doc->'company'->>'businessName' AS company, doc->'address'->>'country' AS country FROM "billingConfiguration" WHERE doc->>'defaultConfiguration' = 'true';
SELECT doc->>'name' AS image_category FROM "imageCategory" ORDER BY id;
