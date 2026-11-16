-- A business partner's address and bank account may be ended on the day
-- before the database's (ADR-0051 As built, hdates; review:batch75).
-- Partner ends them on partner's day, the earliest of the tenant's
-- companies' days in their time zones (batch 74), which in Toronto from
-- 19:00 is UTC's yesterday; platform.end_validity refused such an end as
-- in the past (current_date is UTC's), so ending an address or an account
-- on a Toronto evening failed. No zone is more than a day behind UTC
-- (offsets run from UTC-12), so the trigger allows one day before
-- current_date for these tables, named by the trigger argument
-- 'company_day', as 000198 allows one day after it; partner's own check
-- on partner's day stays the exact rule. Identity's tables keep
-- current_date: theirs is the day authorization judges access on. The
-- function keeps its name, which platform.secure_table reads.
CREATE OR REPLACE FUNCTION platform.end_validity() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  before jsonb;
  after jsonb;
  earliest date := current_date;
BEGIN
  SELECT coalesce(jsonb_object_agg(key, value), '{}') INTO before FROM jsonb_each(to_jsonb(OLD)) WHERE key NOT LIKE '%\_cipher' AND key NOT LIKE '%\_fingerprint';
  SELECT coalesce(jsonb_object_agg(key, value), '{}') INTO after FROM jsonb_each(to_jsonb(NEW)) WHERE key NOT LIKE '%\_cipher' AND key NOT LIKE '%\_fingerprint';
  IF before = after THEN
    RETURN NEW; -- the same value sealed under another key
  END IF;
  IF NEW.valid_to IS NULL OR (OLD.valid_to IS NOT NULL AND NEW.valid_to >= OLD.valid_to) THEN
    RAISE EXCEPTION '%: validity can only be ended or shortened', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF TG_NARGS > 0 AND TG_ARGV[0] = 'company_day' THEN
    earliest := current_date - 1;
  END IF;
  IF NEW.valid_to < earliest THEN
    RAISE EXCEPTION '%: validity cannot be ended in the past', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.ended_by IS NULL THEN
    RAISE EXCEPTION '%: ended_by is required', TG_TABLE_NAME USING ERRCODE = 'not_null_violation';
  END IF;
  NEW.ended_at := now();
  RETURN NEW;
END $$;
DROP TRIGGER address_end ON partner.address;
CREATE TRIGGER address_end BEFORE UPDATE ON partner.address
  FOR EACH ROW WHEN (NOT partner.address_blanked(OLD, NEW)) EXECUTE FUNCTION platform.end_validity('company_day');
DROP TRIGGER bank_account_end ON partner.bank_account;
CREATE TRIGGER bank_account_end BEFORE UPDATE ON partner.bank_account
  FOR EACH ROW WHEN (NOT partner.bank_account_blanked(OLD, NEW)) EXECUTE FUNCTION platform.end_validity('company_day');
