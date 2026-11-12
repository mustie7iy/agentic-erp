-- A billing document (T79a; ADR-0067 §9; 03-data-model §3.29): what a
-- billing clerk bills of one confirmed sales order, the quantities
-- delivered of its items and not yet billed, at the order's prices. An
-- item's amount is the order item's net value for the quantity billed so
-- far, this document included, less what earlier documents billed, so the
-- documents of a fully billed item add up to its net value to the minor
-- unit. A billing document is created `created` and posts nothing;
-- posting it through receivables is T79b, which is granted what it sets.
-- Each item adds its quantity to the order item's billed_quantity, which
-- never exceeds what is delivered (sales_order_item_quantities, 000291),
-- and the document moves the order's version, as a delivery does
-- (000292).
-- Expand only (06 §6.4): two tables and three permissions, which the
-- previous version neither reads nor writes.
CREATE TABLE sales.billing_document (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  document_number  text NOT NULL CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id  uuid NOT NULL,       -- tenancy context
  customer_id      uuid NOT NULL,       -- partner context
  sales_order_id   uuid NOT NULL,
  billing_date     date NOT NULL,
  currency         char(3) NOT NULL REFERENCES ref.currency,
  net_amount       bigint NOT NULL CONSTRAINT billing_document_net_positive CHECK (net_amount > 0),
  status           text NOT NULL CONSTRAINT billing_document_status_check CHECK (status IN ('created')),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT billing_document_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  -- What a document's items reference, so an item is of its document's order.
  CONSTRAINT billing_document_of_order UNIQUE (tenant_id, id, sales_order_id),
  CONSTRAINT billing_document_sales_order_fkey FOREIGN KEY (tenant_id, sales_order_id) REFERENCES sales.sales_order (tenant_id, id)
);
CREATE TRIGGER billing_document_touch BEFORE UPDATE ON sales.billing_document FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: a company's billing documents by date, a customer's and an order's.
CREATE INDEX billing_document_by_date ON sales.billing_document (tenant_id, legal_entity_id, billing_date, id);
CREATE INDEX billing_document_by_customer ON sales.billing_document (tenant_id, customer_id, billing_date, id);
CREATE INDEX billing_document_by_order ON sales.billing_document (tenant_id, sales_order_id);
SELECT platform.secure_table('sales.billing_document'::regclass);
-- A created billing document is not changed or cancelled in the first
-- version; posting (T79b) is granted the columns it sets.
REVOKE UPDATE ON sales.billing_document FROM erp_app;

-- An item of a billing document: a quantity more than zero of one item of
-- the document's order, an order item at most once per document, with
-- its amount in the document's currency, more than zero. What it names
-- and its price are the order item's, which nothing changes.
CREATE TABLE sales.billing_document_item (
  tenant_id            uuid NOT NULL,
  billing_document_id  uuid NOT NULL,
  item_no              integer NOT NULL CHECK (item_no > 0),
  sales_order_id       uuid NOT NULL,
  order_item_no        integer NOT NULL,
  quantity             numeric(20,6) NOT NULL CONSTRAINT billing_document_item_quantity_positive CHECK (quantity > 0),
  net_amount           bigint NOT NULL CONSTRAINT billing_document_item_net_positive CHECK (net_amount > 0),
  PRIMARY KEY (tenant_id, billing_document_id, item_no),
  CONSTRAINT billing_document_item_once UNIQUE (tenant_id, billing_document_id, order_item_no),
  CONSTRAINT billing_document_item_document_fkey FOREIGN KEY (tenant_id, billing_document_id, sales_order_id)
    REFERENCES sales.billing_document (tenant_id, id, sales_order_id),
  CONSTRAINT billing_document_item_order_item_fkey FOREIGN KEY (tenant_id, sales_order_id, order_item_no)
    REFERENCES sales.sales_order_item (tenant_id, sales_order_id, item_no)
);
-- What earlier documents billed of an order item.
CREATE INDEX billing_document_item_by_order_item ON sales.billing_document_item (tenant_id, sales_order_id, order_item_no);
-- What was billed is fixed.
CREATE TRIGGER billing_document_item_immutable BEFORE UPDATE OR DELETE ON sales.billing_document_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.billing_document_item'::regclass);

-- A billing document is of a confirmed order of its company, customer and
-- currency, and moves the order's version once. As the delivery's
-- (000292), the two functions run as the tables' owner, since the
-- application may update neither a confirmed order nor its items;
-- row-level security still applies to that owner. The application reads
-- the order FOR UPDATE first, which serializes the billing documents and
-- deliveries of one order (ADR-0008).
CREATE FUNCTION sales.record_billing_document() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE sales.sales_order SET version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.sales_order_id AND status = 'confirmed'
    AND legal_entity_id = NEW.legal_entity_id AND customer_id = NEW.customer_id AND currency = NEW.currency;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'billing document %: its sales order is not a confirmed one of its company, customer and currency', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_document_order_confirmed';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_billing_document() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_billing_document() FROM PUBLIC;
CREATE TRIGGER billing_document_order_confirmed AFTER INSERT ON sales.billing_document
  FOR EACH ROW EXECUTE FUNCTION sales.record_billing_document();

-- A billing document's item adds its quantity to what is billed of the
-- order item, so that quantity is always the sum of the item's billing
-- documents; more than is delivered is refused by
-- sales_order_item_quantities. The amounts billed of an order item never
-- exceed its net value, and are its net value once it is billed in full.
CREATE FUNCTION sales.record_billing_document_item() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE ordered numeric; billed numeric; net bigint; amounts numeric;
BEGIN
  UPDATE sales.sales_order_item SET billed_quantity = billed_quantity + NEW.quantity
  WHERE tenant_id = NEW.tenant_id AND sales_order_id = NEW.sales_order_id AND item_no = NEW.order_item_no
  RETURNING quantity, billed_quantity, net_amount INTO ordered, billed, net;
  SELECT sum(net_amount) INTO amounts FROM sales.billing_document_item
  WHERE tenant_id = NEW.tenant_id AND sales_order_id = NEW.sales_order_id AND order_item_no = NEW.order_item_no;
  IF amounts > net OR (billed = ordered AND amounts <> net) THEN
    RAISE EXCEPTION 'billing document %: item % of its order would be billed % of its net value %', NEW.billing_document_id, NEW.order_item_no, amounts, net
      USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_document_item_amounts';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_billing_document_item() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_billing_document_item() FROM PUBLIC;
CREATE TRIGGER billing_document_item_billed AFTER INSERT ON sales.billing_document_item
  FOR EACH ROW EXECUTE FUNCTION sales.record_billing_document_item();

-- Reading billing documents, creating them and posting them (ADR-0067
-- §7); posting itself comes with T79b.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.billing.read', 'View billing documents of the companies one may act in', false),
  ('sales.billing.write', 'Create billing documents for the delivered quantities of sales orders', false),
  ('sales.billing.post', 'Post billing documents as customer invoices', false);
