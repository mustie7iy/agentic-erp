-- Sales prices (T80a; ADR-0067 §10; 03-data-model §3.29): the price of an
-- item key in a company, for every customer or for one, per price_unit of
-- a unit of measure in a currency, valid from a day and, when it has a
-- last day, until it. A quotation or sales order item that names the item
-- key without a price takes the record valid on the document's date in
-- its unit and the document's currency: the customer's before the general
-- one, then the latest valid_from, then the one recorded last. A record
-- is never changed or deleted: a new price is a new record. The items say
-- where their price came from (price_source). legal_entity_id is the
-- tenancy context's and customer_id the partner context's: no foreign key
-- leaves the schema (02 §2.1). Expand only (06 §6.4): a new table, two
-- permissions and a column with a default, which the previous version
-- neither reads nor writes; the rows it writes are entered prices, the
-- default.
CREATE TABLE sales.price (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,       -- tenancy context
  item_key         text NOT NULL CONSTRAINT price_item_key_check CHECK (btrim(item_key) <> '' AND length(item_key) <= 40),
  customer_id      uuid,                -- partner context; null: every customer
  uom_code         text NOT NULL REFERENCES ref.unit_of_measure,
  currency         char(3) NOT NULL REFERENCES ref.currency,
  unit_price       bigint NOT NULL CONSTRAINT price_positive CHECK (unit_price > 0),
  price_unit       integer NOT NULL DEFAULT 1 CONSTRAINT price_unit_positive CHECK (price_unit > 0),
  valid_from       date NOT NULL,
  valid_to         date,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT price_validity CHECK (valid_to >= valid_from)
);
-- Determination and the list: an item key's records in a company, the
-- latest first.
CREATE INDEX price_by_item_key ON sales.price (tenant_id, legal_entity_id, item_key, valid_from DESC, id);
CREATE INDEX price_by_customer ON sales.price (tenant_id, customer_id, item_key) WHERE customer_id IS NOT NULL;
-- What was recorded is fixed: the items priced from it name no record, so
-- the record is the only evidence of the price they took.
CREATE TRIGGER price_immutable BEFORE UPDATE OR DELETE ON sales.price
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.price'::regclass);

-- Where an item's price came from: entered with it, or the price record
-- valid on the document's date. The rows there are entered prices.
ALTER TABLE sales.quotation_item ADD COLUMN price_source text NOT NULL DEFAULT 'manual'
  CONSTRAINT quotation_item_price_source_check CHECK (price_source IN ('manual', 'price_list'));
ALTER TABLE sales.sales_order_item ADD COLUMN price_source text NOT NULL DEFAULT 'manual'
  CONSTRAINT sales_order_item_price_source_check CHECK (price_source IN ('manual', 'price_list'));

-- Reading price records and recording them (ADR-0067 §7, §10).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.price.read', 'View sales prices of the companies one may act in', false),
  ('sales.price.write', 'Record sales prices for item keys, for every customer or for one', false);
