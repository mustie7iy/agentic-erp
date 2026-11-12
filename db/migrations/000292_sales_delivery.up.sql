-- A delivery confirmation (T78c; ADR-0067 §8; 03-data-model §3.29): a
-- person records that quantities of a confirmed sales order's items were
-- delivered, or a service made available, on a date. It is kept in the
-- sales context: it moves no stock and posts nothing (stock is outside
-- the required scope); billing and revenue recognition read it. A
-- delivery is written once and never changed or deleted; it is numbered
-- within its order. Each of its items adds its quantity to the order
-- item's delivered_quantity, which never exceeds what was ordered
-- (sales_order_item_quantities, 000291), and the delivery moves the
-- order's version, since the order's representation shows what is
-- delivered.
-- Expand only (06 §6.4): two tables and a permission, which the previous
-- version neither reads nor writes; the order's change rule admits what
-- the previous version never does.
CREATE TABLE sales.delivery (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  sales_order_id  uuid NOT NULL,
  delivery_no     integer NOT NULL CONSTRAINT delivery_no_positive CHECK (delivery_no > 0),
  delivery_date   date NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  created_by      uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT delivery_no_once UNIQUE (tenant_id, sales_order_id, delivery_no),
  -- What a delivery's items reference, so an item is of its delivery's order.
  CONSTRAINT delivery_of_order UNIQUE (tenant_id, id, sales_order_id),
  CONSTRAINT delivery_sales_order_fkey FOREIGN KEY (tenant_id, sales_order_id) REFERENCES sales.sales_order (tenant_id, id)
);
-- The list: an order's deliveries by date.
CREATE INDEX delivery_by_date ON sales.delivery (tenant_id, sales_order_id, delivery_date, id);
CREATE TRIGGER delivery_immutable BEFORE UPDATE OR DELETE ON sales.delivery
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.delivery'::regclass);

-- An item of a delivery: a quantity more than zero of one item of the
-- delivery's order, in the order item's unit; an order item at most once
-- per delivery.
CREATE TABLE sales.delivery_item (
  tenant_id       uuid NOT NULL,
  delivery_id     uuid NOT NULL,
  sales_order_id  uuid NOT NULL,
  order_item_no   integer NOT NULL,
  quantity        numeric(20,6) NOT NULL CONSTRAINT delivery_item_quantity_positive CHECK (quantity > 0),
  PRIMARY KEY (tenant_id, delivery_id, order_item_no),
  CONSTRAINT delivery_item_delivery_fkey FOREIGN KEY (tenant_id, delivery_id, sales_order_id)
    REFERENCES sales.delivery (tenant_id, id, sales_order_id),
  CONSTRAINT delivery_item_order_item_fkey FOREIGN KEY (tenant_id, sales_order_id, order_item_no)
    REFERENCES sales.sales_order_item (tenant_id, sales_order_id, item_no)
);
-- An order item's deliveries, which sales.Orders reads (T81c).
CREATE INDEX delivery_item_by_order_item ON sales.delivery_item (tenant_id, sales_order_id, order_item_no);
CREATE TRIGGER delivery_item_immutable BEFORE UPDATE OR DELETE ON sales.delivery_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.delivery_item'::regclass);

-- A confirmed order keeps its status; its version moves when a delivery
-- (and, from T79a, a billing document) changes what its items show. The
-- application's own updates stay as 000291 left them: a draft is
-- confirmed, once. Only an update made by a trigger of this schema (the
-- delivery's, below) moves a confirmed order's version.
CREATE OR REPLACE FUNCTION sales.check_sales_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'confirmed' AND NEW.status = 'confirmed' AND pg_trigger_depth() > 1 THEN
    RETURN NEW;
  END IF;
  IF OLD.status <> 'draft' OR NEW.status <> 'confirmed' THEN
    RAISE EXCEPTION 'sales order %: only a draft changes, and only by being confirmed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_confirmed_once';
  END IF;
  RETURN NEW;
END $$;

-- Only a confirmed order takes a delivery, and each delivery moves the
-- order's version once. The application may update neither a confirmed
-- order nor its items, so the two functions run as the tables' owner
-- (000011); row-level security still applies to that owner. The
-- application reads the order FOR UPDATE first, which serializes the
-- deliveries of one order (ADR-0008).
CREATE FUNCTION sales.record_delivery() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE sales.sales_order SET version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.sales_order_id AND status = 'confirmed';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'delivery %: its sales order is not confirmed', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'delivery_order_confirmed';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_delivery() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_delivery() FROM PUBLIC;
CREATE TRIGGER delivery_order_confirmed AFTER INSERT ON sales.delivery
  FOR EACH ROW EXECUTE FUNCTION sales.record_delivery();

-- A delivery's item adds its quantity to what is delivered of the order
-- item, so that quantity is always the sum of the item's deliveries; more
-- than was ordered is refused by sales_order_item_quantities.
CREATE FUNCTION sales.record_delivery_item() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE sales.sales_order_item SET delivered_quantity = delivered_quantity + NEW.quantity
  WHERE tenant_id = NEW.tenant_id AND sales_order_id = NEW.sales_order_id AND item_no = NEW.order_item_no;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.record_delivery_item() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION sales.record_delivery_item() FROM PUBLIC;
CREATE TRIGGER delivery_item_delivered AFTER INSERT ON sales.delivery_item
  FOR EACH ROW EXECUTE FUNCTION sales.record_delivery_item();

-- Confirming deliveries (ADR-0067 §7); they are read with the order
-- (sales.order.read).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.delivery.confirm', 'Confirm that quantities of a confirmed sales order were delivered', false);
