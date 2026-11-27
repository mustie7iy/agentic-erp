-- A submitted S&OP plan version, and one that names an approval request,
-- which the previous version cannot hold, is refused rather than dropped
-- (06 §6.4: a down migration loses no data). Otherwise the down drops what
-- 000494 added and restores 000447's trigger.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM mrp.sop_plan WHERE status = 'submitted' OR approval_request_id IS NOT NULL) THEN
    RAISE EXCEPTION 'S&OP plan versions submitted for approval exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a = 'mrp.sop.approve' OR permission_b = 'mrp.sop.approve';
DELETE FROM identity.role_permission WHERE permission_code = 'mrp.sop.approve';
DELETE FROM ref.permission WHERE code = 'mrp.sop.approve';
DROP TRIGGER sop_plan_created ON mrp.sop_plan;
DROP FUNCTION mrp.sop_plan_created();
CREATE OR REPLACE FUNCTION mrp.sop_plan_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'S&OP plan %: a plan version is never deleted', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF OLD.status = 'fixed' THEN
    RAISE EXCEPTION 'S&OP plan %: it is fixed, so it changes no more', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF (NEW.legal_entity_id, NEW.number, NEW.as_of, NEW.first_day, NEW.last_day, NEW.months, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM (OLD.legal_entity_id, OLD.number, OLD.as_of, OLD.first_day, OLD.last_day, OLD.months, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'S&OP plan %: its company, number and horizon do not change', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE mrp.sop_plan
  DROP CONSTRAINT sop_plan_submitted,
  DROP CONSTRAINT sop_plan_status_check,
  ADD CONSTRAINT sop_plan_status_check CHECK (status IN ('draft', 'fixed')),
  DROP COLUMN approval_request_id;
