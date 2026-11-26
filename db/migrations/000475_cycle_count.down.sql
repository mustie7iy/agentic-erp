-- Cycle counting indicators, which the previous version cannot hold, are
-- refused rather than dropped (06 §6.4: a down migration loses no data).
-- A material's indicator names one of its plant's (its foreign key), so
-- no indicator means no material's indicator either.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.cycle_count_indicator) THEN
    RAISE EXCEPTION 'cycle counting indicators exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a = 'inventory.count_plan.write' OR permission_b = 'inventory.count_plan.write';
DELETE FROM identity.role_permission WHERE permission_code = 'inventory.count_plan.write';
DELETE FROM ref.permission WHERE code = 'inventory.count_plan.write';
DROP TABLE inventory.cycle_count_material;
DROP TABLE inventory.cycle_count_indicator;
