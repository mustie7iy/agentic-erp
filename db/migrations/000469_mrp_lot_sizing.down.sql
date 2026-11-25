-- Drops what 000469 added: plans are lot for lot again, and run lines
-- name the shortage alone, as a server before lot sizing reads them.
ALTER TABLE mrp.run_line DROP CONSTRAINT run_line_lots, DROP COLUMN lots, DROP COLUMN proposed, DROP COLUMN lot_size;
ALTER TABLE mrp.material_plan DROP CONSTRAINT material_plan_rounding, DROP CONSTRAINT material_plan_lot_bounds,
  DROP CONSTRAINT material_plan_maximum_level, DROP CONSTRAINT material_plan_lot_size_fields, DROP COLUMN rounding_value,
  DROP COLUMN maximum_lot_size, DROP COLUMN minimum_lot_size, DROP COLUMN maximum_stock_level, DROP COLUMN fixed_lot_size, DROP COLUMN lot_size;
