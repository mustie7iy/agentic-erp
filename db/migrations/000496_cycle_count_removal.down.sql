-- Removed indicators, which the previous version cannot hold (it would
-- read them as materials' indicators), and ABC analyses applied are
-- refused rather than dropped (06 §6.4: a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.cycle_count_material WHERE status = 'removed') THEN
    RAISE EXCEPTION 'removed cycle counting indicators of materials exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
  IF EXISTS (SELECT FROM inventory.cycle_count_abc_application) THEN
    RAISE EXCEPTION 'ABC analyses applied exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
UPDATE ref.permission SET description = 'Maintain cycle counting: the count indicators of plants and the indicators of materials at plants'
  WHERE code = 'inventory.count_plan.write';
DROP TABLE inventory.cycle_count_abc_change;
DROP TABLE inventory.cycle_count_abc_application;
DROP TRIGGER cycle_count_material_removed ON inventory.cycle_count_material;
DROP FUNCTION inventory.cycle_count_material_removed();
DROP INDEX inventory.cycle_count_material_once;
ALTER TABLE inventory.cycle_count_material ADD CONSTRAINT cycle_count_material_once UNIQUE (tenant_id, plant_id, material_id);
ALTER TABLE inventory.cycle_count_material DROP COLUMN status;
