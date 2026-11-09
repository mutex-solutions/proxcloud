package mailoutbox

const schema = `
CREATE TABLE IF NOT EXISTS mail_outbox_event (
 event_key text PRIMARY KEY CHECK(length(event_key)=64),
 key_id text NOT NULL,
 fingerprint bytea NOT NULL CHECK(octet_length(fingerprint)=32)
);
CREATE TABLE IF NOT EXISTS mail_outbox (
 id uuid PRIMARY KEY,
 event_key text NOT NULL REFERENCES mail_outbox_event(event_key),
 recipient_key text NOT NULL CHECK(length(recipient_key)=64),
 kind text NOT NULL CHECK(kind='savings-reminder/v1'),
 version integer NOT NULL CHECK(version=1),
 key_id text NOT NULL,
 sealed bytea NOT NULL CHECK(octet_length(sealed) BETWEEN 29 AND 1500000),
 fingerprint bytea NOT NULL CHECK(octet_length(fingerprint)=32),
 state text NOT NULL CHECK(state IN ('pending','claimed','DISPATCHING','accepted','UNKNOWN','dead','blocked_config','suppressed','no_recipient','expired')),
 owner uuid,
 epoch bigint NOT NULL DEFAULT 0,
 attempt_id uuid,
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 8),
 lease_until timestamptz,
 next_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 reason text NOT NULL DEFAULT '' CHECK(reason IN ('','expired','blocked_config','suppressed','no_recipient','relay_accepted','network','smtp_temporary','smtp_permanent','configuration','tls_required','cancelled','receipt_lost','sealed_payload','dispatch_lease_expired','legacy_untyped')),
 UNIQUE(event_key,recipient_key)
);
-- Controlled upgrade of the original, automatically named single-kind CHECK.
-- Ensure holds the schema advisory lock and commits this together with all DDL.
DO $$ DECLARE old_check text; BEGIN
 SELECT pg_get_expr(conbin,conrelid) INTO old_check FROM pg_constraint
 WHERE conrelid='mail_outbox'::regclass AND conname='mail_outbox_kind_check';
 IF old_check IS NOT NULL THEN
  IF old_check <> '(kind = ''savings-reminder/v1''::text)' THEN
   RAISE EXCEPTION 'mail outbox unexpected family constraint';
  END IF;
  ALTER TABLE mail_outbox DROP CONSTRAINT mail_outbox_kind_check;
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='mail_outbox'::regclass AND conname='mail_outbox_family_v1') THEN
  ALTER TABLE mail_outbox ADD CONSTRAINT mail_outbox_family_v1
   CHECK(kind IN ('savings-reminder/v1','project-invite/v1') AND version=1);
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS mail_outbox_claim ON mail_outbox(next_at,id) WHERE state IN ('pending','claimed');
CREATE INDEX IF NOT EXISTS mail_outbox_intent_expiry ON mail_outbox(lease_until) WHERE state='DISPATCHING';
CREATE TABLE IF NOT EXISTS mail_outbox_attempt (
 outbox_id uuid NOT NULL REFERENCES mail_outbox(id),
 attempt_id uuid NOT NULL,
 epoch bigint NOT NULL,
 phase text NOT NULL CHECK(phase IN ('intent','unknown','receipt')),
 delivery text NOT NULL DEFAULT '' CHECK(delivery IN ('','accepted','known_not_accepted','unknown')),
 stage text NOT NULL DEFAULT '',
 class text NOT NULL DEFAULT '',
 permanent boolean NOT NULL DEFAULT false,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(attempt_id,phase)
);
CREATE OR REPLACE FUNCTION mail_outbox_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_TABLE_NAME='mail_outbox' AND TG_OP='UPDATE' THEN
  IF (NEW.id,NEW.event_key,NEW.recipient_key,NEW.kind,NEW.version,NEW.key_id,NEW.sealed,NEW.fingerprint,NEW.created_at,NEW.expires_at)
    IS NOT DISTINCT FROM (OLD.id,OLD.event_key,OLD.recipient_key,OLD.kind,OLD.version,OLD.key_id,OLD.sealed,OLD.fingerprint,OLD.created_at,OLD.expires_at) THEN RETURN NEW; END IF;
 END IF;
 RAISE EXCEPTION 'mail outbox immutable data';
END $$;
DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='mail_outbox_immutable_payload' AND tgrelid='mail_outbox'::regclass) THEN
  CREATE TRIGGER mail_outbox_immutable_payload BEFORE UPDATE OR DELETE ON mail_outbox FOR EACH ROW EXECUTE FUNCTION mail_outbox_immutable();
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='mail_outbox_immutable_attempt' AND tgrelid='mail_outbox_attempt'::regclass) THEN
  CREATE TRIGGER mail_outbox_immutable_attempt BEFORE UPDATE OR DELETE ON mail_outbox_attempt FOR EACH ROW EXECUTE FUNCTION mail_outbox_immutable();
 END IF;
 IF NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgname='mail_outbox_immutable_event' AND tgrelid='mail_outbox_event'::regclass) THEN
  CREATE TRIGGER mail_outbox_immutable_event BEFORE UPDATE OR DELETE ON mail_outbox_event FOR EACH ROW EXECUTE FUNCTION mail_outbox_immutable();
 END IF;
END $$;
`
