-- Period lot sizes, which the previous version cannot hold, are refused
-- rather than dropped (06 §6.4: a down migration loses no data): a plan
-- or a run line sized by period. Only such a line has lots in
-- run_line_lot, so with neither the table is empty and the down drops
-- what 000506 added.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM mrp.material_plan WHERE lot_size IN ('daily', 'weekly', 'monthly'))
     OR EXISTS (SELECT FROM mrp.run_line WHERE lot_size IN ('daily', 'weekly', 'monthly'))
     OR EXISTS (SELECT FROM mrp.run_line_lot) THEN
    RAISE EXCEPTION 'period lot sizing of material plans or run lines exists; the previous version cannot hold it' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE mrp.run_line_lot;
DROP FUNCTION mrp.check_run_line_lot();
ALTER TABLE mrp.run_line
  DROP CONSTRAINT run_line_lot_size_check,
  ADD CONSTRAINT run_line_lot_size_check CHECK (lot_size IN ('lot_for_lot', 'fixed', 'replenish_to_maximum'));
ALTER TABLE mrp.material_plan
  DROP CONSTRAINT material_plan_lot_size_check,
  ADD CONSTRAINT material_plan_lot_size_check CHECK (lot_size IN ('lot_for_lot', 'fixed', 'replenish_to_maximum'));
