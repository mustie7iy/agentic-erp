-- A returns order (T85a; ADR-0067, As built T85a; 03-data-model §3.29):
-- quantities of a posted billing document's items that the customer
-- sends back, with a reason. It is a document of the sales context
-- alone: it carries no amounts, posts nothing and moves no stock (stock
-- is outside the required scope); the credit for a return is a credit
-- memo request's (T85c). It is a draft when created and is confirmed
-- once; it is not changed or cancelled in the first version, so every
-- returns order, a draft included, takes its quantities of what the
-- billing document's items billed. Each item adds its quantity to the
-- sales order item's returned_quantity, which never exceeds what is
-- billed, and the returns order moves the sales order's version, since
-- the order's representation shows what is returned, as a delivery and
-- a billing document do (000292, 000293).
-- Expand only (06 §6.4): a column with a default, two tables and three
-- permissions, which the previous version neither reads nor writes; an
-- order item it inserts has nothing returned.
ALTER TABLE sales.sales_order_item
  ADD COLUMN returned_quantity numeric(20,6) NOT NULL DEFAULT 0,
  ADD CONSTRAINT sales_order_item_returned CHECK (returned_quantity >= 0 AND returned_quantity <= billed_quantity);

CREATE TABLE sales.returns_order (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  document_number      text NOT NULL CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id      uuid NOT NULL,       -- tenancy context
  customer_id          uuid NOT NULL,       -- partner context
  billing_document_id  uuid NOT NULL,
  sales_order_id       uuid NOT NULL,
  returns_date         date NOT NULL,
  reason               text NOT NULL CONSTRAINT returns_order_reason_check CHECK (reason IN ('defective', 'wrong_item', 'not_wanted', 'other')),
  reason_text          text CONSTRAINT returns_order_reason_text_check CHECK (btrim(reason_text) <> '' AND length(reason_text) <= 500),
  status               text NOT NULL CONSTRAINT returns_order_status_check CHECK (status IN ('draft', 'confirmed')),
  version              integer NOT NULL DEFAULT 1,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT returns_order_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  -- What a returns order's items reference, so an item is of its order's billing document.
  CONSTRAINT returns_order_of_document UNIQUE (tenant_id, id, billing_document_id),
  -- The billing document with its sales order (billing_document_of_order, 000293).
  CONSTRAINT returns_order_billing_document_fkey FOREIGN KEY (tenant_id, billing_document_id, sales_order_id)
    REFERENCES sales.billing_document (tenant_id, id, sales_order_id)
);
CREATE TRIGGER returns_order_touch BEFORE UPDATE ON sales.returns_order FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: a company's returns orders by date, a customer's, a billing document's and a sales order's.
CREATE INDEX returns_order_by_date ON sales.returns_order (tenant_id, legal_entity_id, returns_date, id);
CREATE INDEX returns_order_by_customer ON sales.returns_order (tenant_id, customer_id, returns_date, id);
CREATE INDEX returns_order_by_document ON sales.returns_order (tenant_id, billing_document_id);
CREATE INDEX returns_order_by_order ON sales.returns_order (tenant_id, sales_order_id);
SELECT platform.secure_table('sales.returns_order'::regclass);

-- A returns order is for a posted billing document of its company and
-- customer, and moves the sales order's version once. As the delivery's
-- (000292), the function runs as the tables' owner, since the
-- application may not update a confirmed order; row-level security
-- still applies to that owner. The application reads the sales order
-- FOR UPDATE first, which serializes the returns orders of one order
-- (ADR-0008).
CREATE FUNCTION sales.record_returns_order() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF NOT EXISTS (SELECT FROM sales.billing_document b
      WHERE b.tenant_id = NEW.tenant_id AND b.id = NEW.billing_document_id AND b.status = 'posted'
        AND b.legal_entity_id = NEW.legal_entity_id AND b.customer_id = NEW.customer_id) THEN
    RAISE EXCEPTION 'returns order %: its billing document is not a posted one of its company and customer', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'returns_order_document_posted';
  END IF;
  -- A posted billing document's order is confirmed (billing_document_order_confirmed) and stays so.
  UPDATE sales.sales_order SET version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.sales_order_id;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_returns_order() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_returns_order() FROM PUBLIC;
CREATE TRIGGER returns_order_document_posted AFTER INSERT ON sales.returns_order
  FOR EACH ROW EXECUTE FUNCTION sales.record_returns_order();

-- Confirming is the only change: a draft is confirmed, once, and what is
-- returned stays (the grant names the columns confirming sets; nothing is
-- deleted).
CREATE FUNCTION sales.check_returns_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' OR NEW.status <> 'confirmed' THEN
    RAISE EXCEPTION 'returns order %: only a draft changes, and only by being confirmed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'returns_order_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_returns_order_change() OWNER TO erp_owner;
CREATE TRIGGER returns_order_confirmed_once BEFORE UPDATE ON sales.returns_order
  FOR EACH ROW EXECUTE FUNCTION sales.check_returns_order_change();
REVOKE UPDATE ON sales.returns_order FROM erp_app;
GRANT UPDATE (status, version, updated_by) ON sales.returns_order TO erp_app;

-- An item of a returns order: a quantity more than zero of one item of
-- the order's billing document, a billing item at most once per returns
-- order. What it names, its unit and the sales order's item are the
-- billing document item's, which nothing changes. The application may
-- neither update nor delete an item (the table has no version, so it is
-- granted neither; 000011).
CREATE TABLE sales.returns_order_item (
  tenant_id            uuid NOT NULL,
  returns_order_id     uuid NOT NULL,
  item_no              integer NOT NULL CHECK (item_no > 0),
  billing_document_id  uuid NOT NULL,
  billing_item_no      integer NOT NULL,
  quantity             numeric(20,6) NOT NULL CONSTRAINT returns_order_item_quantity_positive CHECK (quantity > 0),
  PRIMARY KEY (tenant_id, returns_order_id, item_no),
  CONSTRAINT returns_order_item_once UNIQUE (tenant_id, returns_order_id, billing_item_no),
  CONSTRAINT returns_order_item_order_fkey FOREIGN KEY (tenant_id, returns_order_id, billing_document_id)
    REFERENCES sales.returns_order (tenant_id, id, billing_document_id),
  CONSTRAINT returns_order_item_billing_item_fkey FOREIGN KEY (tenant_id, billing_document_id, billing_item_no)
    REFERENCES sales.billing_document_item (tenant_id, billing_document_id, item_no)
);
-- What the returns orders of a billing document took of one of its items.
CREATE INDEX returns_order_item_by_billing_item ON sales.returns_order_item (tenant_id, billing_document_id, billing_item_no);
SELECT platform.secure_table('sales.returns_order_item'::regclass);

-- A returns order's item adds its quantity to what is returned of the
-- sales order item its billing item billed, so that quantity is always
-- the sum of the item's returns; the returns orders of a billing
-- document never take more of one of its items than it billed.
CREATE FUNCTION sales.record_returns_order_item() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE o uuid; n integer; billed numeric; taken numeric;
BEGIN
  SELECT b.sales_order_id, b.order_item_no, b.quantity INTO o, n, billed FROM sales.billing_document_item b
  WHERE b.tenant_id = NEW.tenant_id AND b.billing_document_id = NEW.billing_document_id AND b.item_no = NEW.billing_item_no;
  SELECT sum(r.quantity) INTO taken FROM sales.returns_order_item r
  WHERE r.tenant_id = NEW.tenant_id AND r.billing_document_id = NEW.billing_document_id AND r.billing_item_no = NEW.billing_item_no;
  IF taken > billed THEN
    RAISE EXCEPTION 'returns order %: item % of its billing document would have % returned of % billed', NEW.returns_order_id, NEW.billing_item_no, taken, billed
      USING ERRCODE = 'check_violation', CONSTRAINT = 'returns_order_item_quantities';
  END IF;
  UPDATE sales.sales_order_item SET returned_quantity = returned_quantity + NEW.quantity
  WHERE tenant_id = NEW.tenant_id AND sales_order_id = o AND item_no = n;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_returns_order_item() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_returns_order_item() FROM PUBLIC;
CREATE TRIGGER returns_order_item_returned AFTER INSERT ON sales.returns_order_item
  FOR EACH ROW EXECUTE FUNCTION sales.record_returns_order_item();

-- Reading returns orders, creating them and confirming them (ADR-0067
-- §7); none is an approval permission.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.return.read', 'View returns orders of the companies one may act in', false),
  ('sales.return.write', 'Create returns orders for the items of posted billing documents', false),
  ('sales.return.confirm', 'Confirm returns orders', false);
