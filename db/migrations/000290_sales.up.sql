-- Sales (T78a; ADR-0067 §1-§4; 02 §2.2, 03-data-model §3.29): the sales
-- context's schema with its outbox and inbox, and the quotation a sales
-- representative makes to a customer: a header valid until a date and its
-- items, each a quantity of something described (an optional free-text
-- item key, no material master) at a unit price per price unit, with the
-- revenue account and output tax code its billing will post with. Amounts
-- are net: tax is calculated when the order is billed (ADR-0067 §3). A
-- quotation is open, accepted or rejected and what was offered is never
-- changed. legal_entity_id is the tenancy context's, customer_id the
-- partner context's, the items' accounts the ledger's: no foreign key
-- leaves the schema (02 §2.1). Expand only: a new schema and two
-- permissions, which the previous version neither reads nor writes
-- (06 §6.4).
CREATE SCHEMA sales;
GRANT USAGE ON SCHEMA sales TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('sales');
SELECT platform.secure_table('sales.outbox'::regclass);
SELECT platform.secure_table('sales.inbox'::regclass);
GRANT UPDATE (published_at) ON sales.outbox TO erp_app;
GRANT USAGE ON SCHEMA sales TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON sales.outbox TO erp_relay;

-- A quotation: what the company offers a customer, valid until a date
-- (ADR-0067 §2). net_amount is the sum of the items' values in minor
-- units of currency.
CREATE TABLE sales.quotation (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  document_number  text NOT NULL CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id  uuid NOT NULL,       -- tenancy context
  customer_id      uuid NOT NULL,       -- partner context
  quotation_date   date NOT NULL,
  valid_until      date NOT NULL,
  currency         char(3) NOT NULL REFERENCES ref.currency,
  net_amount       bigint NOT NULL CONSTRAINT quotation_net_positive CHECK (net_amount > 0),
  status           text NOT NULL CONSTRAINT quotation_status_check CHECK (status IN ('open', 'accepted', 'rejected')),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT quotation_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  CONSTRAINT quotation_validity CHECK (valid_until >= quotation_date)
);
CREATE TRIGGER quotation_touch BEFORE UPDATE ON sales.quotation FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: a company's quotations by date, and a customer's.
CREATE INDEX quotation_by_date ON sales.quotation (tenant_id, legal_entity_id, quotation_date, id);
CREATE INDEX quotation_by_customer ON sales.quotation (tenant_id, customer_id, quotation_date, id);
SELECT platform.secure_table('sales.quotation'::regclass);

-- The customer's answer is the only change: an open quotation is accepted
-- or rejected, once, and what was offered stays (the grant names the
-- columns the answer sets; nothing is deleted).
CREATE FUNCTION sales.check_quotation_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'open' OR NEW.status NOT IN ('accepted', 'rejected') THEN
    RAISE EXCEPTION 'quotation %: only an open quotation changes, and only by being accepted or rejected', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'quotation_answered_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_quotation_change() OWNER TO erp_owner;
CREATE TRIGGER quotation_answered_once BEFORE UPDATE ON sales.quotation
  FOR EACH ROW EXECUTE FUNCTION sales.check_quotation_change();
REVOKE UPDATE ON sales.quotation FROM erp_app;
GRANT UPDATE (status, version, updated_by) ON sales.quotation TO erp_app;

-- An item: a quantity in a unit of measure of what is sold, named by a
-- description and an optional free-text item key (prices are found by it
-- from T80a), at unit_price per price_unit; net_amount is quantity x
-- unit_price / price_unit rounded half away from zero, more than zero.
-- account_id is the revenue account and tax_code the output tax code the
-- item's billing posts with; an item without a code bears no tax.
CREATE TABLE sales.quotation_item (
  tenant_id     uuid NOT NULL,
  quotation_id  uuid NOT NULL,
  item_no       integer NOT NULL CHECK (item_no > 0),
  item_key      text CONSTRAINT quotation_item_item_key_check CHECK (btrim(item_key) <> '' AND length(item_key) <= 40),
  description   text NOT NULL CONSTRAINT quotation_item_description_check CHECK (btrim(description) <> '' AND length(description) <= 200),
  quantity      numeric(20,6) NOT NULL CONSTRAINT quotation_item_quantity_positive CHECK (quantity > 0),
  uom_code      text NOT NULL REFERENCES ref.unit_of_measure,
  unit_price    bigint NOT NULL CONSTRAINT quotation_item_price_positive CHECK (unit_price > 0),
  price_unit    integer NOT NULL DEFAULT 1 CONSTRAINT quotation_item_price_unit_positive CHECK (price_unit > 0),
  net_amount    bigint NOT NULL CONSTRAINT quotation_item_net_positive CHECK (net_amount > 0),
  account_id    uuid NOT NULL,          -- ledger context
  tax_code      text CHECK (length(tax_code) BETWEEN 1 AND 20),
  PRIMARY KEY (tenant_id, quotation_id, item_no),
  FOREIGN KEY (tenant_id, quotation_id) REFERENCES sales.quotation (tenant_id, id)
);
-- What was offered is fixed.
CREATE TRIGGER quotation_item_immutable BEFORE UPDATE OR DELETE ON sales.quotation_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.quotation_item'::regclass);

-- Reading quotations, and making them and recording the customer's answer
-- (ADR-0067 §7).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.quotation.read', 'View quotations of the companies one may act in', false),
  ('sales.quotation.write', 'Create quotations and record that the customer accepted or rejected them', false);
