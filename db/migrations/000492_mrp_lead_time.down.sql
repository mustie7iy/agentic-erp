-- Replenishment lead times, which the previous version cannot hold, are
-- refused rather than dropped (06 §6.4: a down migration loses no data;
-- batch 96S integration). Otherwise the down drops what 000492 added: the
-- availability check counts the stock alone again, as the previous
-- version does.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM mrp.material_plan WHERE replenishment_lead_time_days IS NOT NULL) THEN
    RAISE EXCEPTION 'material plans name replenishment lead times; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
ALTER TABLE mrp.material_plan DROP COLUMN replenishment_lead_time_days;
