SET LOCAL row_security = off; -- the down migration deletes every tenant's discounts (06 §6.4)
ALTER TABLE sales.sales_order_item DROP COLUMN conditions;
ALTER TABLE sales.quotation_item DROP COLUMN conditions;
DROP INDEX sales.price_by_condition;
-- Discounts are no prices: the previous version would read them as
-- prices without a price, so they go with the columns that made them.
ALTER TABLE sales.price DISABLE TRIGGER price_immutable;
DELETE FROM sales.price WHERE percent IS NOT NULL;
ALTER TABLE sales.price ENABLE TRIGGER price_immutable;
ALTER TABLE sales.price
  DROP CONSTRAINT price_or_percent,
  DROP COLUMN percent,
  DROP COLUMN condition_type,
  ALTER COLUMN price_unit SET DEFAULT 1,
  ALTER COLUMN price_unit SET NOT NULL,
  ALTER COLUMN unit_price SET NOT NULL,
  ALTER COLUMN currency SET NOT NULL,
  ALTER COLUMN uom_code SET NOT NULL;
