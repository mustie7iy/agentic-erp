-- Lot sizing, which the previous version cannot hold, is refused (06 §6.4:
-- a down migration loses no data): a plan sized otherwise than lot for
-- lot, or bounded or rounded, and a run line that proposed other than its
-- shortage in one lot. Otherwise the down drops what 000486 added.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM mrp.material_plan WHERE lot_size <> 'lot_for_lot' OR minimum_lot_size IS NOT NULL OR maximum_lot_size IS NOT NULL
       OR rounding_value IS NOT NULL)
     OR EXISTS (SELECT FROM mrp.run_line WHERE lot_size <> 'lot_for_lot' OR lots > 1 OR proposed <> shortage) THEN
    RAISE EXCEPTION 'lot sizing of material plans or run lines exists; the previous version cannot hold it' USING ERRCODE = 'check_violation';
  END IF;
END $$;
-- Drops what 000486 added: plans are lot for lot again, and run lines
-- name the shortage alone, as a server before lot sizing reads them.
ALTER TABLE mrp.run_line DROP CONSTRAINT run_line_lots, DROP COLUMN lots, DROP COLUMN proposed, DROP COLUMN lot_size;
ALTER TABLE mrp.material_plan DROP CONSTRAINT material_plan_rounding, DROP CONSTRAINT material_plan_lot_bounds,
  DROP CONSTRAINT material_plan_maximum_level, DROP CONSTRAINT material_plan_lot_size_fields, DROP COLUMN rounding_value,
  DROP COLUMN maximum_lot_size, DROP COLUMN minimum_lot_size, DROP COLUMN maximum_stock_level, DROP COLUMN fixed_lot_size, DROP COLUMN lot_size;
