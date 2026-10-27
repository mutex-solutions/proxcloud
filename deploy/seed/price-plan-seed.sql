-- Public rate card seed (docs/pricing-rate-card.md). Idempotent:
--   * plan + rules use fixed ids + ON CONFLICT DO NOTHING
--   * billingConfiguration (base currency USD) inserts only when the table is empty
-- Apply (operator, one command):
--   kubectl --context <ctx> -n <ns> exec deploy/proxcloud-api -- sh -c 'psql "$PROXCLOUD_DB_URL"' < deploy/seed/price-plan-seed.sql
-- Verify afterwards: admin UI -> System -> Price plans, and the provider GPU tab's
-- "Unpriced flavors" list (flavors with a gap bill zero).

CREATE TABLE IF NOT EXISTS "pricePlan" (id text PRIMARY KEY, doc jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS "pricePlanRule" (id text PRIMARY KEY, doc jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS "billingConfiguration" (id text PRIMARY KEY, doc jsonb NOT NULL);

-- Base currency (prices below are USD). Only when no billing configuration exists yet.
INSERT INTO "billingConfiguration" (id, doc)
SELECT '6a4e63e8aa5e5eed10000018', '{"defaultConfiguration": true, "baseCurrency": "USD"}'::jsonb
WHERE NOT EXISTS (SELECT 1 FROM "billingConfiguration");

INSERT INTO "pricePlan" (id, doc) VALUES
('6a4e63e8aa5e5eed10000001', '{"name":"Public rate card","enabled":true,"accessMode":"PUBLIC","serviceProviders":[]}')
ON CONFLICT (id) DO NOTHING;

INSERT INTO "pricePlanRule" (id, doc) VALUES
('6a4e63e8aa5e5eed10000002', '{"name":"Compute:CPU + RAM components","timeUnit":"hour","resourceType":"instance","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"vcpus","tiers":[{"value":"0.008"}]},{"attributeName":"ram_gb","tiers":[{"value":"0.004"}]}],"filters":[],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000003', '{"name":"GPU:nvidia-4090 (RTX 4090)","timeUnit":"hour","resourceType":"instance","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"gpu_count","tiers":[{"value":"0.65"}]}],"filters":[{"attributeName":"gpu_model","operator":"eq","value":"nvidia-4090"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000004', '{"name":"GPU:nvidia-3090 (RTX 3090)","timeUnit":"hour","resourceType":"instance","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"gpu_count","tiers":[{"value":"0.43"}]}],"filters":[{"attributeName":"gpu_model","operator":"eq","value":"nvidia-3090"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000005', '{"name":"GPU:nvidia-3080ti (RTX 3080 Ti, interpolated)","timeUnit":"hour","resourceType":"instance","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"gpu_count","tiers":[{"value":"0.29"}]}],"filters":[{"attributeName":"gpu_model","operator":"eq","value":"nvidia-3080ti"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000006', '{"name":"GPU:nvidia-a6000 (RTX A6000 Ampere 48GB)","timeUnit":"hour","resourceType":"instance","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"gpu_count","tiers":[{"value":"0.47"}]}],"filters":[{"attributeName":"gpu_model","operator":"eq","value":"nvidia-a6000"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000007', '{"name":"GPU:nvidia-pro-6000 (RTX PRO 6000 Blackwell 96GB)","timeUnit":"hour","resourceType":"instance","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"gpu_count","tiers":[{"value":"1.99"}]}],"filters":[{"attributeName":"gpu_model","operator":"eq","value":"nvidia-pro-6000"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000008', '{"name":"GPU:nvidia-pro-4500 (RTX PRO 4500 Blackwell 32GB, interpolated)","timeUnit":"hour","resourceType":"instance","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"gpu_count","tiers":[{"value":"0.69"}]}],"filters":[{"attributeName":"gpu_model","operator":"eq","value":"nvidia-pro-4500"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000009', '{"name":"GPU:intel-a60 (placeholder)","timeUnit":"hour","resourceType":"instance","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"gpu_count","tiers":[{"value":"0.15"}]}],"filters":[{"attributeName":"gpu_model","operator":"eq","value":"intel-a60"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000010', '{"name":"Public egress:1 TiB free, then $0.01/GB","timeUnit":"hour","resourceType":"instance_traffic","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"OVERWRITE_TOTAL","prices":[{"attributeName":"outgoing_public_traffic_mb","tiers":[{"from":"0","to":"1048576","value":"0"},{"from":"1048576","value":"0.00001"}]}],"filters":[],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000011', '{"name":"Block storage","timeUnit":"hour","resourceType":"volume","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"size","tiers":[{"value":"0.000137"}]}],"filters":[],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000012', '{"name":"Floating IP","timeUnit":"hour","resourceType":"floating_ip","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"existence","tiers":[{"value":"0.005"}]}],"filters":[],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000013', '{"name":"Load balancer","timeUnit":"hour","resourceType":"load_balancer","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"existence","tiers":[{"value":"0.0165"}]}],"filters":[],"modifiers":[]}')
ON CONFLICT (id) DO NOTHING;

-- Managed Kubernetes CONTROL-PLANE fee (kubernetes_cluster, priced per cp_replicas).
-- Deliberately commented: the CP-pricing decision is still open (design plan §6 —
-- free CP + paid HA vs flat fee). Uncomment after that decision; keep in sync with the
-- kubernetes_cluster entry in price-plan-seed.json. Worker VMs/volumes/LBs already bill via
-- the instance/volume/load_balancer rules above — never add k8s worker rules here.
-- INSERT INTO "pricePlanRule" (id, doc) VALUES
-- ('6a4e63e8aa5e5eed10000014', '{"name":"Kubernetes control plane (per CP replica)","timeUnit":"hour","resourceType":"kubernetes_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"cp_replicas","tiers":[{"value":"0.015"}]}],"filters":[],"modifiers":[]}')
-- ON CONFLICT (id) DO NOTHING;

-- Managed Database FULL price (database_cluster: pre-multiplied totals — vcpus_total /
-- ram_gb_total / storage_gb_total = per-instance x replicas; the DB pods/volumes run on
-- ops-owned nodes so nothing else bills them; the endpoint gate DEFERS accrual until the
-- Octavia VIP exists, then back-bills the current cycle). One rule per engine, rates =
-- DIGITALOCEAN PARITY (see the derivation note in price-plan-seed.json; keep both in sync).
INSERT INTO "pricePlanRule" (id, doc) VALUES
('6a4e63e8aa5e5eed10000015', '{"name":"Managed Database: PostgreSQL (DigitalOcean-parity)","timeUnit":"hour","resourceType":"database_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"vcpus_total","tiers":[{"value":"0.002740"}]},{"attributeName":"ram_gb_total","tiers":[{"value":"0.015068"}]},{"attributeName":"storage_gb_total","tiers":[{"value":"0.000295"}]}],"filters":[{"attributeName":"engine","operator":"eq","value":"postgresql"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000016', '{"name":"Managed Database: MySQL (DigitalOcean-parity)","timeUnit":"hour","resourceType":"database_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"vcpus_total","tiers":[{"value":"0.002740"}]},{"attributeName":"ram_gb_total","tiers":[{"value":"0.015068"}]},{"attributeName":"storage_gb_total","tiers":[{"value":"0.000295"}]}],"filters":[{"attributeName":"engine","operator":"eq","value":"mysql"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000017', '{"name":"Managed Database: MariaDB (MySQL DigitalOcean-parity)","timeUnit":"hour","resourceType":"database_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"vcpus_total","tiers":[{"value":"0.002740"}]},{"attributeName":"ram_gb_total","tiers":[{"value":"0.015068"}]},{"attributeName":"storage_gb_total","tiers":[{"value":"0.000295"}]}],"filters":[{"attributeName":"engine","operator":"eq","value":"mariadb"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000018', '{"name":"Managed Database: Valkey (DigitalOcean-parity)","timeUnit":"hour","resourceType":"database_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"vcpus_total","tiers":[{"value":"0.002945"}]},{"attributeName":"ram_gb_total","tiers":[{"value":"0.014658"}]},{"attributeName":"storage_gb_total","tiers":[{"value":"0.000295"}]}],"filters":[{"attributeName":"engine","operator":"eq","value":"valkey"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000019', '{"name":"Managed Database: FerretDB (MongoDB DigitalOcean-parity)","timeUnit":"hour","resourceType":"database_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"vcpus_total","tiers":[{"value":"0.002740"}]},{"attributeName":"ram_gb_total","tiers":[{"value":"0.015068"}]},{"attributeName":"storage_gb_total","tiers":[{"value":"0.000295"}]}],"filters":[{"attributeName":"engine","operator":"eq","value":"ferretdb"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000020', '{"name":"Managed Database: Kafka (DigitalOcean-parity, RAM-rated)","timeUnit":"hour","resourceType":"database_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"ram_gb_total","tiers":[{"value":"0.028767"}]},{"attributeName":"storage_gb_total","tiers":[{"value":"0.000295"}]}],"filters":[{"attributeName":"engine","operator":"eq","value":"kafka"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000021', '{"name":"Managed Database: OpenSearch (DigitalOcean-parity)","timeUnit":"hour","resourceType":"database_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"vcpus_total","tiers":[{"value":"0.002740"}]},{"attributeName":"ram_gb_total","tiers":[{"value":"0.006164"}]},{"attributeName":"storage_gb_total","tiers":[{"value":"0.000295"}]}],"filters":[{"attributeName":"engine","operator":"eq","value":"opensearch"}],"modifiers":[]}'),
('6a4e63e8aa5e5eed10000022', '{"name":"Managed Database: autoscale surcharge (per database while enabled)","timeUnit":"hour","resourceType":"database_cluster","pricePlanId":"6a4e63e8aa5e5eed10000001","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"autoscale_enabled","tiers":[{"value":"0.0137"}]}],"filters":[],"modifiers":[]}')
ON CONFLICT (id) DO NOTHING;

-- Sanity read-back
SELECT id, doc->>'name' AS name FROM "pricePlan" WHERE id = '6a4e63e8aa5e5eed10000001';
SELECT count(*) AS rules FROM "pricePlanRule" WHERE doc->>'pricePlanId' = '6a4e63e8aa5e5eed10000001';
SELECT doc->>'baseCurrency' AS base_currency FROM "billingConfiguration" LIMIT 1;
