-- Lot sizing, which the previous version cannot hold, is refused rather
-- than dropped (06 §6.4: a down migration loses no data): a plan sized
-- otherwise than lot for lot, or bounded or rounded, and a run line that
-- proposed other than its shortage in one lot.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM mrp.material_plan WHERE lot_size <> 'lot_for_lot' OR minimum_lot_size IS NOT NULL OR maximum_lot_size IS NOT NULL
       OR rounding_value IS NOT NULL)
     OR EXISTS (SELECT FROM mrp.run_line WHERE lot_size <> 'lot_for_lot' OR lots > 1 OR proposed <> shortage) THEN
    RAISE EXCEPTION 'lot sizing of material plans or run lines exists; the previous version cannot hold it' USING ERRCODE = 'check_violation';
  END IF;
END $$;
