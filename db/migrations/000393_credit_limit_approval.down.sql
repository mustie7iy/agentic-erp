DELETE FROM identity.sod_rule WHERE permission_a = 'credit.limit.approve' OR permission_b = 'credit.limit.approve';
DELETE FROM identity.role_permission WHERE permission_code = 'credit.limit.approve';
DELETE FROM ref.permission WHERE code = 'credit.limit.approve';
DROP TABLE credit.credit_limit_change;
DROP FUNCTION credit.credit_limit_change_decided_once();
DROP TRIGGER credit_limit_status_guard ON credit.credit_limit;
DROP FUNCTION credit.credit_limit_status_guard();
REVOKE UPDATE (status) ON credit.credit_limit FROM erp_app;
-- A pending limit would read as one in effect: its create change is
-- decided, or the limit removed, first.
DO $$
BEGIN
  IF EXISTS (SELECT FROM credit.credit_limit WHERE status = 'pending') THEN
    RAISE EXCEPTION 'credit limits wait for their approval; decide them before downgrading';
  END IF;
END $$;
ALTER TABLE credit.credit_limit DROP COLUMN status;
