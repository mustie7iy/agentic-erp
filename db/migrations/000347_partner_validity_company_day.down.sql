DROP TRIGGER address_end ON partner.address;
CREATE TRIGGER address_end BEFORE UPDATE ON partner.address
  FOR EACH ROW WHEN (NOT partner.address_blanked(OLD, NEW)) EXECUTE FUNCTION platform.end_validity();
DROP TRIGGER bank_account_end ON partner.bank_account;
CREATE TRIGGER bank_account_end BEFORE UPDATE ON partner.bank_account
  FOR EACH ROW WHEN (NOT partner.bank_account_blanked(OLD, NEW)) EXECUTE FUNCTION platform.end_validity();
CREATE OR REPLACE FUNCTION platform.end_validity() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  before jsonb;
  after jsonb;
BEGIN
  SELECT coalesce(jsonb_object_agg(key, value), '{}') INTO before FROM jsonb_each(to_jsonb(OLD)) WHERE key NOT LIKE '%\_cipher' AND key NOT LIKE '%\_fingerprint';
  SELECT coalesce(jsonb_object_agg(key, value), '{}') INTO after FROM jsonb_each(to_jsonb(NEW)) WHERE key NOT LIKE '%\_cipher' AND key NOT LIKE '%\_fingerprint';
  IF before = after THEN
    RETURN NEW; -- the same value sealed under another key
  END IF;
  IF NEW.valid_to IS NULL OR (OLD.valid_to IS NOT NULL AND NEW.valid_to >= OLD.valid_to) THEN
    RAISE EXCEPTION '%: validity can only be ended or shortened', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.valid_to < current_date THEN
    RAISE EXCEPTION '%: validity cannot be ended in the past', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.ended_by IS NULL THEN
    RAISE EXCEPTION '%: ended_by is required', TG_TABLE_NAME USING ERRCODE = 'not_null_violation';
  END IF;
  NEW.ended_at := now();
  RETURN NEW;
END $$;
