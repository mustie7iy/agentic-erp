-- Scales on a price record (T80e; ADR-0067 §12; 03-data-model §3.29): a
-- price or a discount may have scale levels, read by the item's quantity
-- or, for a discount, by the item's value before it. A level starts at a
-- quantity in the scale's unit or a value in the scale's currency, more
-- than zero, and gives the rate of the record's kind: a price per the
-- record's price unit in its currency, or a discount's percent. An item
-- takes the rate of the highest level whose start its quantity or value
-- reaches, and the record's own rate below the first. A price's scale is
-- by quantity in its own unit; a discount's names the unit or the
-- currency it is in, and an item in another does not take the record.
-- Levels are written with their record and never changed, as the record
-- is not. Expand only (06 §6.4): columns the previous version neither
-- reads nor writes, null on its rows, and a new table.
ALTER TABLE sales.price
  ADD COLUMN scale_basis text CONSTRAINT price_scale_basis_check CHECK (scale_basis IN ('quantity', 'value')),
  ADD COLUMN scale_uom_code text REFERENCES ref.unit_of_measure,
  ADD COLUMN scale_currency char(3) REFERENCES ref.currency,
  -- A scale states what it is read in: a quantity scale its unit, a value
  -- scale its currency, and only a discount has a value scale.
  -- (coalesce: a null basis must not leave the check unknown, which
  -- passes it.)
  ADD CONSTRAINT price_scale_stated CHECK (CASE coalesce(scale_basis, '')
    WHEN '' THEN scale_uom_code IS NULL AND scale_currency IS NULL
    WHEN 'quantity' THEN scale_uom_code IS NOT NULL AND scale_currency IS NULL
    ELSE percent IS NOT NULL AND scale_currency IS NOT NULL AND scale_uom_code IS NULL END),
  -- A price's quantity scale is in the price's own unit.
  ADD CONSTRAINT price_scale_unit CHECK (percent IS NOT NULL OR scale_uom_code IS NULL OR scale_uom_code = uom_code);

CREATE TABLE sales.price_scale (
  tenant_id     uuid NOT NULL,
  price_id      uuid NOT NULL,
  level         smallint NOT NULL CONSTRAINT price_scale_level_check CHECK (level BETWEEN 1 AND 20),
  from_quantity numeric(20,6) CONSTRAINT price_scale_from_quantity_check CHECK (from_quantity > 0),
  from_amount   bigint CONSTRAINT price_scale_from_amount_check CHECK (from_amount > 0),
  unit_price    bigint CONSTRAINT price_scale_unit_price_check CHECK (unit_price > 0),
  percent       numeric(9,6) CONSTRAINT price_scale_percent_check CHECK (percent > 0 AND percent < 100),
  PRIMARY KEY (tenant_id, price_id, level),
  FOREIGN KEY (tenant_id, price_id) REFERENCES sales.price (tenant_id, id),
  -- A level starts at a quantity or at a value, and gives one rate.
  CONSTRAINT price_scale_from CHECK ((from_quantity IS NULL) <> (from_amount IS NULL)),
  CONSTRAINT price_scale_rate CHECK ((unit_price IS NULL) <> (percent IS NULL))
);
-- No two levels of a record start at the same place, so the level an
-- item reaches is one.
CREATE UNIQUE INDEX price_scale_by_quantity ON sales.price_scale (tenant_id, price_id, from_quantity) WHERE from_quantity IS NOT NULL;
CREATE UNIQUE INDEX price_scale_by_amount ON sales.price_scale (tenant_id, price_id, from_amount) WHERE from_amount IS NOT NULL;
CREATE TRIGGER price_scale_immutable BEFORE UPDATE OR DELETE ON sales.price_scale
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.price_scale'::regclass);
