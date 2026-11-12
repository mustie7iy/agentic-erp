-- Sales conditions (T80b; ADR-0067 §11; 03-data-model §3.28): a price
-- record may name a condition type of the tenant's configuration
-- (pricing.condition_types), and a record of a type of kind
-- discount_percent is a discount: a percentage of an item's value, more
-- than 0 and less than 100, with no unit, currency or price. A sales item
-- keeps the conditions the pricing procedure applied when it was created:
-- the price and each discount with its rate and amount, in the
-- procedure's order; their amounts add up to its net value. The
-- configuration is the configuration context's: a record's condition type
-- has no foreign key (02 §2.1), and a type is never removed once
-- activated. Expand only (06 §6.4): columns the previous version does not
-- write, a price's columns still written by it as before, and a default
-- for the items it writes, which carry no conditions.
ALTER TABLE sales.price
  ADD COLUMN condition_type text CONSTRAINT price_condition_type_check CHECK (condition_type ~ '^[A-Z0-9_]{1,10}$'),
  ADD COLUMN percent numeric(9,6) CONSTRAINT price_percent_range CHECK (percent > 0 AND percent < 100),
  ALTER COLUMN uom_code DROP NOT NULL,
  ALTER COLUMN currency DROP NOT NULL,
  ALTER COLUMN unit_price DROP NOT NULL,
  ALTER COLUMN price_unit DROP NOT NULL,
  ALTER COLUMN price_unit DROP DEFAULT,
  -- A record is a price (a unit, a currency, a price per a price unit) or
  -- a discount of a condition type (a percent), never a mix.
  ADD CONSTRAINT price_or_percent CHECK (
    (percent IS NULL AND uom_code IS NOT NULL AND currency IS NOT NULL AND unit_price IS NOT NULL AND price_unit IS NOT NULL)
    OR (percent IS NOT NULL AND condition_type IS NOT NULL AND uom_code IS NULL AND currency IS NULL AND unit_price IS NULL AND price_unit IS NULL));
-- The discounts of an item key by their type, the latest first.
CREATE INDEX price_by_condition ON sales.price (tenant_id, legal_entity_id, item_key, condition_type, valid_from DESC, id)
  WHERE percent IS NOT NULL;

-- The conditions applied to an item, in the procedure's order: a JSON
-- array, empty for an item priced without a procedure. Each element has
-- condition_type, name, kind and amount (minor units of the document's
-- currency, a discount's negative), a price its unit_price and
-- price_unit, a discount its percent.
ALTER TABLE sales.quotation_item ADD COLUMN conditions jsonb NOT NULL DEFAULT '[]'
  CONSTRAINT quotation_item_conditions_array CHECK (jsonb_typeof(conditions) = 'array');
ALTER TABLE sales.sales_order_item ADD COLUMN conditions jsonb NOT NULL DEFAULT '[]'
  CONSTRAINT sales_order_item_conditions_array CHECK (jsonb_typeof(conditions) = 'array');
