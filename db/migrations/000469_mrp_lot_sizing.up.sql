-- Lot sizing procedures (ADR-0084 As built, lot sizing; 03-data-model
-- §3.37): a material's planning data at a plant names how a run sizes the
-- quantity it proposes for a shortage (SAP MARC-DISLS, from memory):
-- lot_for_lot (EX), the shortage itself, as every run did before;
-- fixed (FX), as many lots of fixed_lot_size as cover the shortage; or
-- replenish_to_maximum (HB), what brings the available quantity up to
-- maximum_stock_level. A lot-for-lot or replenish-to-maximum quantity is
-- raised to minimum_lot_size (BSTMI), rounded up to a multiple of
-- rounding_value (BSTRF) and split into lots of at most maximum_lot_size
-- (BSTMA); a fixed lot is used as it is, so those three are not given
-- with it. Every quantity is in the material's base unit.
--
-- A run's line names the procedure it sized by, the quantity it proposed
-- and in how many lots (the items of the one requisition it proposes for
-- the material).
--
-- Expand only (06 §6.4): new columns, nullable or with a constant
-- default, so a server of the release before writes rows these checks
-- accept: its plans are lot for lot, and its run lines name no proposed
-- quantity or lots, which readers take as the shortage in one lot (lot
-- for lot, as it proposed). Lines are insert-only, so none is rewritten.
ALTER TABLE mrp.material_plan
  ADD COLUMN lot_size text NOT NULL DEFAULT 'lot_for_lot'
    CONSTRAINT material_plan_lot_size_check CHECK (lot_size IN ('lot_for_lot', 'fixed', 'replenish_to_maximum')),
  ADD COLUMN fixed_lot_size numeric(20,6) CONSTRAINT material_plan_fixed_lot_size_check CHECK (fixed_lot_size > 0),
  ADD COLUMN maximum_stock_level numeric(20,6) CONSTRAINT material_plan_maximum_stock_level_check CHECK (maximum_stock_level > 0),
  ADD COLUMN minimum_lot_size numeric(20,6) CONSTRAINT material_plan_minimum_lot_size_check CHECK (minimum_lot_size > 0),
  ADD COLUMN maximum_lot_size numeric(20,6) CONSTRAINT material_plan_maximum_lot_size_check CHECK (maximum_lot_size > 0),
  ADD COLUMN rounding_value numeric(20,6) CONSTRAINT material_plan_rounding_value_check CHECK (rounding_value > 0),
  -- The fixed lot size is the fixed procedure's and the maximum stock
  -- level the replenish-to-maximum procedure's, given exactly with them; a
  -- fixed lot is not bounded or rounded.
  ADD CONSTRAINT material_plan_lot_size_fields CHECK ((lot_size = 'fixed') = (fixed_lot_size IS NOT NULL)
    AND (lot_size = 'replenish_to_maximum') = (maximum_stock_level IS NOT NULL)
    AND (lot_size <> 'fixed' OR (minimum_lot_size IS NULL AND maximum_lot_size IS NULL AND rounding_value IS NULL))),
  -- Replenishing to a level below the reorder point would propose less
  -- than the shortage.
  ADD CONSTRAINT material_plan_maximum_level CHECK (maximum_stock_level >= reorder_point),
  ADD CONSTRAINT material_plan_lot_bounds CHECK (minimum_lot_size <= maximum_lot_size),
  -- Bounds that are multiples of the rounding value keep every rounded
  -- lot within them.
  ADD CONSTRAINT material_plan_rounding CHECK (minimum_lot_size % rounding_value = 0 AND maximum_lot_size % rounding_value = 0);

ALTER TABLE mrp.run_line
  ADD COLUMN lot_size text NOT NULL DEFAULT 'lot_for_lot'
    CONSTRAINT run_line_lot_size_check CHECK (lot_size IN ('lot_for_lot', 'fixed', 'replenish_to_maximum')),
  ADD COLUMN proposed numeric(20,6),
  ADD COLUMN lots integer,
  -- A line of a server before names neither. Otherwise what is proposed
  -- covers the shortage, and is proposed exactly when there is one, in
  -- one lot or more.
  ADD CONSTRAINT run_line_lots CHECK (proposed IS NULL AND lots IS NULL
    OR proposed IS NOT NULL AND proposed >= shortage AND (proposed > 0) = (shortage > 0) AND (lots IS NOT NULL) = (proposed > 0)
      AND (lots IS NULL OR lots >= 1));
