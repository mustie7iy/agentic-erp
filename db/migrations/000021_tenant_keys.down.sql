REVOKE UPDATE (account_cipher, account_fingerprint) ON partner.bank_account FROM erp_app;
CREATE OR REPLACE FUNCTION platform.end_validity() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
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
DROP TABLE tenancy.tenant_key;
