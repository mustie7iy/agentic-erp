-- A sales order (T78b; ADR-0067 §5; 03-data-model §3.28): what a customer
-- ordered, created from an accepted quotation, whose customer, currency
-- and items it copies, or directly from items of the quotation's shape.
-- One accepted quotation gives one order. The order keeps the payment
-- term its billing posts with: the one given with it, else the
-- customer's in the company when it was created. It is a draft when
-- created and is confirmed once; it is not changed or cancelled in the
-- first version. Each item shows what was ordered, delivered and billed;
-- the delivery and billing slices (T78c, T79a) move the last two.
-- Expand only (06 §6.4): two tables and three permissions, which the
-- previous version neither reads nor writes.
CREATE TABLE sales.sales_order (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  document_number    text NOT NULL CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id    uuid NOT NULL,     -- tenancy context
  customer_id        uuid NOT NULL,     -- partner context
  quotation_id       uuid,              -- the accepted quotation it was created from
  order_date         date NOT NULL,
  currency           char(3) NOT NULL REFERENCES ref.currency,
  payment_term_code  text NOT NULL CHECK (length(payment_term_code) BETWEEN 1 AND 20),  -- partner context
  net_amount         bigint NOT NULL CONSTRAINT sales_order_net_positive CHECK (net_amount > 0),
  status             text NOT NULL CONSTRAINT sales_order_status_check CHECK (status IN ('draft', 'confirmed')),
  version            integer NOT NULL DEFAULT 1,
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT sales_order_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  CONSTRAINT sales_order_quotation_fkey FOREIGN KEY (tenant_id, quotation_id) REFERENCES sales.quotation (tenant_id, id)
);
-- One accepted quotation gives one order.
CREATE UNIQUE INDEX sales_order_one_per_quotation ON sales.sales_order (tenant_id, quotation_id) WHERE quotation_id IS NOT NULL;
CREATE TRIGGER sales_order_touch BEFORE UPDATE ON sales.sales_order FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: a company's orders by date, and a customer's.
CREATE INDEX sales_order_by_date ON sales.sales_order (tenant_id, legal_entity_id, order_date, id);
CREATE INDEX sales_order_by_customer ON sales.sales_order (tenant_id, customer_id, order_date, id);
SELECT platform.secure_table('sales.sales_order'::regclass);

-- An order is created from an accepted quotation of its company and
-- customer, in its currency.
CREATE FUNCTION sales.check_sales_order_quotation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.quotation_id IS NOT NULL AND NOT EXISTS (SELECT FROM sales.quotation q
      WHERE q.tenant_id = NEW.tenant_id AND q.id = NEW.quotation_id AND q.status = 'accepted'
        AND q.legal_entity_id = NEW.legal_entity_id AND q.customer_id = NEW.customer_id AND q.currency = NEW.currency) THEN
    RAISE EXCEPTION 'sales order %: its quotation is not an accepted one of its company, customer and currency', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_quotation_accepted';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_sales_order_quotation() OWNER TO erp_owner;
CREATE TRIGGER sales_order_quotation_accepted BEFORE INSERT ON sales.sales_order
  FOR EACH ROW EXECUTE FUNCTION sales.check_sales_order_quotation();

-- Confirming is the only change: a draft is confirmed, once, and what was
-- ordered stays (the grant names the columns confirming sets; nothing is
-- deleted).
CREATE FUNCTION sales.check_sales_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' OR NEW.status <> 'confirmed' THEN
    RAISE EXCEPTION 'sales order %: only a draft changes, and only by being confirmed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_sales_order_change() OWNER TO erp_owner;
CREATE TRIGGER sales_order_confirmed_once BEFORE UPDATE ON sales.sales_order
  FOR EACH ROW EXECUTE FUNCTION sales.check_sales_order_change();
REVOKE UPDATE ON sales.sales_order FROM erp_app;
GRANT UPDATE (status, version, updated_by) ON sales.sales_order TO erp_app;

-- An item of a sales order: a quotation item's shape (000276), with the
-- quantities delivered and billed so far, which never exceed what was
-- ordered and delivered. Nothing changes an item yet: the slices that
-- confirm deliveries and bill them are granted what they set (T78c,
-- T79a).
CREATE TABLE sales.sales_order_item (
  tenant_id           uuid NOT NULL,
  sales_order_id      uuid NOT NULL,
  item_no             integer NOT NULL CHECK (item_no > 0),
  item_key            text CONSTRAINT sales_order_item_item_key_check CHECK (btrim(item_key) <> '' AND length(item_key) <= 40),
  description         text NOT NULL CONSTRAINT sales_order_item_description_check CHECK (btrim(description) <> '' AND length(description) <= 200),
  quantity            numeric(20,6) NOT NULL CONSTRAINT sales_order_item_quantity_positive CHECK (quantity > 0),
  uom_code            text NOT NULL REFERENCES ref.unit_of_measure,
  unit_price          bigint NOT NULL CONSTRAINT sales_order_item_price_positive CHECK (unit_price > 0),
  price_unit          integer NOT NULL DEFAULT 1 CONSTRAINT sales_order_item_price_unit_positive CHECK (price_unit > 0),
  net_amount          bigint NOT NULL CONSTRAINT sales_order_item_net_positive CHECK (net_amount > 0),
  account_id          uuid NOT NULL,    -- ledger context
  tax_code            text CHECK (length(tax_code) BETWEEN 1 AND 20),
  delivered_quantity  numeric(20,6) NOT NULL DEFAULT 0,
  billed_quantity     numeric(20,6) NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, sales_order_id, item_no),
  FOREIGN KEY (tenant_id, sales_order_id) REFERENCES sales.sales_order (tenant_id, id),
  CONSTRAINT sales_order_item_quantities CHECK (billed_quantity >= 0 AND billed_quantity <= delivered_quantity AND delivered_quantity <= quantity)
);
SELECT platform.secure_table('sales.sales_order_item'::regclass);
REVOKE UPDATE ON sales.sales_order_item FROM erp_app;

-- Reading sales orders, creating them and confirming them (ADR-0067 §7).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.order.read', 'View sales orders of the companies one may act in', false),
  ('sales.order.write', 'Create sales orders, from an accepted quotation or directly', false),
  ('sales.order.confirm', 'Confirm sales orders', false);
