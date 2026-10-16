DELETE FROM identity.sod_rule WHERE 'ledger.period.unlock.approve' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'ledger.period.unlock.approve';
DELETE FROM ref.permission WHERE code = 'ledger.period.unlock.approve';
CREATE OR REPLACE FUNCTION ledger.check_period_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.status, NEW.status) IN (('open', 'closed'), ('closed', 'open'), ('closed', 'locked')) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'period %/% cannot go from % to %', OLD.fiscal_year, OLD.period, OLD.status, NEW.status USING ERRCODE = 'check_violation';
END $$;
ALTER TABLE ledger.period_status DROP CONSTRAINT period_status_unlock_only_locked;
ALTER TABLE ledger.period_status DROP COLUMN unlock_request_id;
