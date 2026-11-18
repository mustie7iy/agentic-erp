SET LOCAL row_security = off; -- the down migration deletes every tenant's scaled records (06 §6.4)
-- The previous version would price every item of a scaled record at its
-- own rate, whatever the quantity or value, so the scaled records go with
-- the columns that made them scaled.
DROP TABLE sales.price_scale;
ALTER TABLE sales.price DISABLE TRIGGER price_immutable;
DELETE FROM sales.price WHERE scale_basis IS NOT NULL;
ALTER TABLE sales.price ENABLE TRIGGER price_immutable;
ALTER TABLE sales.price
  DROP CONSTRAINT price_scale_unit,
  DROP CONSTRAINT price_scale_stated,
  DROP COLUMN scale_currency,
  DROP COLUMN scale_uom_code,
  DROP COLUMN scale_basis;
