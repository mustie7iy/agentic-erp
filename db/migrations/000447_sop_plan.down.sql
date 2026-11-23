-- S&OP plan versions the previous version cannot hold are refused rather
-- than dropped (06 §6.4: a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM mrp.sop_plan) THEN
    RAISE EXCEPTION 'S&OP plan versions exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a IN ('mrp.sop.read', 'mrp.sop.write') OR permission_b IN ('mrp.sop.read', 'mrp.sop.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('mrp.sop.read', 'mrp.sop.write');
DELETE FROM ref.permission WHERE code IN ('mrp.sop.read', 'mrp.sop.write');
DROP TABLE mrp.sop_plan_line;
DROP TABLE mrp.sop_plan;
DROP FUNCTION mrp.sop_plan_line_fixed();
DROP FUNCTION mrp.sop_plan_fixed();
