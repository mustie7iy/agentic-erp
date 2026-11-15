-- Deletion runs (T100c; ADR-0079 §12; 03-data-model §3.6, §3.32): the
-- privacy officer previews and starts a run that blanks the personal data
-- of each business partner whose personal data has been blocked longer
-- than the company's retention rule for personal data. Blanking replaces
-- each inventory field of the partner by a fixed marker and ends its bank
-- accounts, irreversibly; no row is removed, and no ledger document or
-- audit event is touched (audit.event_immutable does not change). Expand
-- only (06 §6.4): a nullable column, a check every existing row meets,
-- triggers that let through only an update the previous version never
-- makes, new tables, new column grants and one permission.

-- When the partner's personal data was blanked, null while it is not. It
-- is set only on a partner whose personal data is blocked, and once set
-- neither it nor the block changes again.
ALTER TABLE partner.business_partner
  ADD COLUMN personal_data_deleted_at timestamptz,
  ADD CONSTRAINT business_partner_personal_data_deleted_check CHECK (personal_data_deleted_at IS NULL OR personal_data_blocked_at IS NOT NULL);

CREATE FUNCTION partner.check_personal_data_deleted() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.personal_data_deleted_at IS NOT NULL AND (NEW.personal_data_deleted_at IS DISTINCT FROM OLD.personal_data_deleted_at
      OR NEW.personal_data_blocked_at IS DISTINCT FROM OLD.personal_data_blocked_at OR NEW.status <> OLD.status
      OR NEW.name <> OLD.name OR NEW.legal_name IS DISTINCT FROM OLD.legal_name) THEN
    RAISE EXCEPTION 'business partner %: its personal data was deleted; it is not unblocked or renamed (business_partner_personal_data_deleted_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'business_partner_personal_data_deleted_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION partner.check_personal_data_deleted() OWNER TO erp_owner;
CREATE TRIGGER business_partner_personal_data_deleted_fixed BEFORE UPDATE ON partner.business_partner
  FOR EACH ROW EXECUTE FUNCTION partner.check_personal_data_deleted();

-- Blanking an address: its lines, city, postal code and region become the
-- marker (a value that had none keeps none) and nothing else changes. It
-- is the one change of an address besides ending it (platform.end_validity),
-- which it does not go through.
CREATE FUNCTION partner.address_blanked(o partner.address, n partner.address) RETURNS boolean
LANGUAGE sql STABLE AS $$
  SELECT n.line1 = '[deleted]' AND n.city = '[deleted]'
    AND n.line2 IS NOT DISTINCT FROM CASE WHEN o.line2 IS NOT NULL THEN '[deleted]' END
    AND n.postal_code IS NOT DISTINCT FROM CASE WHEN o.postal_code IS NOT NULL THEN '[deleted]' END
    AND n.region IS NOT DISTINCT FROM CASE WHEN o.region IS NOT NULL THEN '[deleted]' END
    AND to_jsonb(o) - ARRAY['line1', 'line2', 'city', 'postal_code', 'region'] = to_jsonb(n) - ARRAY['line1', 'line2', 'city', 'postal_code', 'region']
$$;
ALTER FUNCTION partner.address_blanked(partner.address, partner.address) OWNER TO erp_owner;

-- Blanking a bank account: its holder becomes the marker, its last four
-- characters XXXX, its BIC none and a Canadian account's institution and
-- transit numbers zeros (their check requires them); its sealed account
-- and fingerprint are the application's (the marker sealed), and its
-- validity may be ended in the same update, which platform.end_validity
-- then checks.
CREATE FUNCTION partner.bank_account_blanked(o partner.bank_account, n partner.bank_account) RETURNS boolean
LANGUAGE sql STABLE AS $$
  SELECT n.holder_name = '[deleted]' AND n.account_last4 = 'XXXX' AND n.bic IS NULL
    AND n.institution_number IS NOT DISTINCT FROM CASE WHEN o.institution_number IS NOT NULL THEN '000' END
    AND n.transit_number IS NOT DISTINCT FROM CASE WHEN o.transit_number IS NOT NULL THEN '00000' END
    AND to_jsonb(o) - ARRAY['holder_name', 'account_last4', 'bic', 'institution_number', 'transit_number', 'account_cipher', 'account_fingerprint']
      = to_jsonb(n) - ARRAY['holder_name', 'account_last4', 'bic', 'institution_number', 'transit_number', 'account_cipher', 'account_fingerprint']
$$;
ALTER FUNCTION partner.bank_account_blanked(partner.bank_account, partner.bank_account) OWNER TO erp_owner;

-- The personal columns of an address or a bank account change only by
-- blanking, and only of a partner whose personal data is deleted (the
-- deletion run sets personal_data_deleted_at first, in the same
-- transaction): the column grants below let the application write them
-- for that alone.
CREATE FUNCTION partner.check_personal_columns() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  deleted boolean;
BEGIN
  IF TG_TABLE_NAME = 'address' THEN
    IF (OLD.line1, OLD.line2, OLD.city, OLD.postal_code, OLD.region) IS NOT DISTINCT FROM (NEW.line1, NEW.line2, NEW.city, NEW.postal_code, NEW.region) THEN
      RETURN NEW;
    END IF;
  ELSIF (OLD.holder_name, OLD.account_last4, OLD.bic, OLD.institution_number, OLD.transit_number)
      IS NOT DISTINCT FROM (NEW.holder_name, NEW.account_last4, NEW.bic, NEW.institution_number, NEW.transit_number) THEN
    RETURN NEW;
  END IF;
  SELECT p.personal_data_deleted_at IS NOT NULL INTO deleted FROM partner.business_partner p WHERE p.tenant_id = NEW.tenant_id AND p.id = NEW.partner_id;
  IF NOT coalesce(deleted, false) THEN
    RAISE EXCEPTION '% %: personal data is blanked only once its partner''s is deleted (personal_data_not_deleted)', TG_TABLE_NAME, OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'personal_data_not_deleted';
  END IF;
  IF TG_TABLE_NAME = 'address' THEN
    IF (OLD.line1, OLD.line2, OLD.city, OLD.postal_code, OLD.region) IS DISTINCT FROM (NEW.line1, NEW.line2, NEW.city, NEW.postal_code, NEW.region)
        AND NOT (NEW.line1 = '[deleted]' AND NEW.city = '[deleted]'
          AND NEW.line2 IS NOT DISTINCT FROM CASE WHEN OLD.line2 IS NOT NULL THEN '[deleted]' END
          AND NEW.postal_code IS NOT DISTINCT FROM CASE WHEN OLD.postal_code IS NOT NULL THEN '[deleted]' END
          AND NEW.region IS NOT DISTINCT FROM CASE WHEN OLD.region IS NOT NULL THEN '[deleted]' END) THEN
      RAISE EXCEPTION 'address %: its lines change only when blanked (address_personal_fixed)', OLD.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'address_personal_fixed';
    END IF;
  ELSIF (OLD.holder_name, OLD.account_last4, OLD.bic, OLD.institution_number, OLD.transit_number)
        IS DISTINCT FROM (NEW.holder_name, NEW.account_last4, NEW.bic, NEW.institution_number, NEW.transit_number)
      AND NOT (NEW.holder_name = '[deleted]' AND NEW.account_last4 = 'XXXX' AND NEW.bic IS NULL
        AND NEW.institution_number IS NOT DISTINCT FROM CASE WHEN OLD.institution_number IS NOT NULL THEN '000' END
        AND NEW.transit_number IS NOT DISTINCT FROM CASE WHEN OLD.transit_number IS NOT NULL THEN '00000' END) THEN
    RAISE EXCEPTION 'bank account %: its holder and identifiers change only when blanked (bank_account_personal_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'bank_account_personal_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION partner.check_personal_columns() OWNER TO erp_owner;
CREATE TRIGGER address_personal_fixed BEFORE UPDATE ON partner.address FOR EACH ROW EXECUTE FUNCTION partner.check_personal_columns();
CREATE TRIGGER bank_account_personal_fixed BEFORE UPDATE ON partner.bank_account FOR EACH ROW EXECUTE FUNCTION partner.check_personal_columns();

-- Blanking alone passes the validity trigger; any other update is held to
-- it as before.
DROP TRIGGER address_end ON partner.address;
CREATE TRIGGER address_end BEFORE UPDATE ON partner.address
  FOR EACH ROW WHEN (NOT partner.address_blanked(OLD, NEW)) EXECUTE FUNCTION platform.end_validity();
DROP TRIGGER bank_account_end ON partner.bank_account;
CREATE TRIGGER bank_account_end BEFORE UPDATE ON partner.bank_account
  FOR EACH ROW WHEN (NOT partner.bank_account_blanked(OLD, NEW)) EXECUTE FUNCTION platform.end_validity();
GRANT UPDATE (line1, line2, city, postal_code, region) ON partner.address TO erp_app;
GRANT UPDATE (holder_name, account_last4, bic, institution_number, transit_number) ON partner.bank_account TO erp_app;

-- A deletion run (ADR-0079 §12): the subjects eligible when it was asked
-- for, under the configuration version given, blanked one by one by the
-- deleter (records.Deleter), each in a transaction of its own. It is
-- queued, then completed, or failed with the problem code that stopped it;
-- either is its last change.
CREATE TABLE records.deletion_run (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  config_version  integer NOT NULL CONSTRAINT deletion_run_config_check CHECK (config_version >= 1),
  status          text NOT NULL DEFAULT 'queued' CONSTRAINT deletion_run_status_check CHECK (status IN ('queued', 'completed', 'failed')),
  subject_count   integer NOT NULL CONSTRAINT deletion_run_count_check CHECK (subject_count >= 1),
  failure_code    text,
  completed_at    timestamptz,
  requested_by    uuid NOT NULL,     -- identity context: the person who asked for it
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  version         integer NOT NULL DEFAULT 1 CONSTRAINT deletion_run_version_check CHECK (version >= 1),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT deletion_run_completed_check CHECK ((status = 'completed') = (completed_at IS NOT NULL)),
  CONSTRAINT deletion_run_failed_check CHECK ((status = 'failed') = (failure_code IS NOT NULL))
);
CREATE TRIGGER deletion_run_touch BEFORE UPDATE ON records.deletion_run FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- One queued run per tenant: a second request answers it.
CREATE UNIQUE INDEX deletion_run_one_queued ON records.deletion_run (tenant_id) WHERE status = 'queued';
CREATE INDEX deletion_run_by_created ON records.deletion_run (tenant_id, created_at DESC, id DESC);
SELECT platform.secure_table('records.deletion_run'::regclass);

CREATE FUNCTION records.check_deletion_run_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'queued' THEN
    RAISE EXCEPTION 'deletion run %: a % run is not changed (deletion_run_settled_fixed)', OLD.id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'deletion_run_settled_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION records.check_deletion_run_change() OWNER TO erp_owner;
CREATE TRIGGER deletion_run_settled_fixed BEFORE UPDATE ON records.deletion_run
  FOR EACH ROW EXECUTE FUNCTION records.check_deletion_run_change();
REVOKE UPDATE, DELETE ON records.deletion_run FROM erp_app;
GRANT UPDATE (status, failure_code, completed_at, version) ON records.deletion_run TO erp_app;
GRANT SELECT (tenant_id, status) ON records.deletion_run TO erp_worker;

-- A subject of a run: a business partner (partner context) by id and
-- number, when its personal data was blocked and the retention it was held
-- to. It is pending, then blanked, or skipped when its block was lifted or
-- renewed since the run was asked for; either is its last change.
CREATE TABLE records.deletion_run_subject (
  tenant_id         uuid NOT NULL,
  run_id            uuid NOT NULL,
  partner_id        uuid NOT NULL,     -- partner context
  partner_number    text NOT NULL,
  blocked_at        timestamptz NOT NULL,
  retention_years   smallint NOT NULL CONSTRAINT deletion_run_subject_retention_check CHECK (retention_years BETWEEN 0 AND 99),
  retention_ends    date NOT NULL,
  status            text NOT NULL DEFAULT 'pending' CONSTRAINT deletion_run_subject_status_check CHECK (status IN ('pending', 'blanked', 'skipped')),
  settled_at        timestamptz,
  PRIMARY KEY (tenant_id, run_id, partner_id),
  FOREIGN KEY (tenant_id, run_id) REFERENCES records.deletion_run (tenant_id, id),
  CONSTRAINT deletion_run_subject_settled_check CHECK ((status = 'pending') = (settled_at IS NULL))
);
CREATE INDEX deletion_run_subject_pending ON records.deletion_run_subject (tenant_id, run_id, partner_number) WHERE status = 'pending';
SELECT platform.secure_table('records.deletion_run_subject'::regclass);

CREATE FUNCTION records.check_deletion_subject_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'pending' THEN
    RAISE EXCEPTION 'deletion run subject %: a % subject is not changed (deletion_run_subject_settled_fixed)', OLD.partner_id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'deletion_run_subject_settled_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION records.check_deletion_subject_change() OWNER TO erp_owner;
CREATE TRIGGER deletion_run_subject_settled_fixed BEFORE UPDATE ON records.deletion_run_subject
  FOR EACH ROW EXECUTE FUNCTION records.check_deletion_subject_change();
REVOKE UPDATE, DELETE ON records.deletion_run_subject FROM erp_app;
GRANT UPDATE (status, settled_at) ON records.deletion_run_subject TO erp_app;

-- Previewing and starting deletion runs (ADR-0079 §12).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('records.privacy.delete', 'Preview and start runs that blank the personal data of blocked business partners past retention', false);
