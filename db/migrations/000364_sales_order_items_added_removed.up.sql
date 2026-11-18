-- Sales order items added and removed, the payment term changed (T78e;
-- ADR-0067, As built T78e; 03-data-model §3.29). An order that is not
-- cancelled takes new items in a change, numbered after its highest, and
-- an item of which nothing is delivered or billed is removed in a change,
-- as SAP's VA02 adds an item and deletes one without follow-on
-- documents. The application deletes no business data (000011), so a
-- removed item stays, marked with the change that removed it, its values
-- as they were and out of the order's value. The order's payment term
-- changes in a change too, while nothing of the order is billed (a
-- billing document reads its order's term). Each is kept in the change:
-- its payment term before and after, and per item its kind of change in
-- the items' JSON.
--
-- Expand only (06 §6.4): nullable columns, an insert rule that the
-- previous version's writes keep (it inserts items with their order,
-- before any change) and the grants a change needs; the previous version neither
-- reads nor writes the columns, and the orders it creates, changes and
-- cancels stay valid.
ALTER TABLE sales.sales_order_item
  -- The change that removed it: sales_order_item_change requires the order's next kept change, so a number above 0.
  ADD COLUMN removed_in_change integer,
  -- A removed item has nothing delivered, billed or returned: none is taken against it afterwards either.
  ADD CONSTRAINT sales_order_item_removed_undelivered CHECK (removed_in_change IS NULL OR delivered_quantity = 0);

ALTER TABLE sales.sales_order_change
  ADD COLUMN payment_term_code_before text,
  ADD COLUMN payment_term_code_after text,
  ADD CONSTRAINT sales_order_change_payment_term CHECK ((payment_term_code_before IS NULL) = (payment_term_code_after IS NULL));

-- An item is written with its order, before any change of it is kept,
-- or, once the order is changed, only in the transaction that keeps the
-- order's next change (T78e), numbered after the order's highest item;
-- never to a cancelled order. The application writes an order's own
-- items while it is a draft and adds every later one with its change.
CREATE FUNCTION sales.check_sales_order_item_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  o record;
BEGIN
  SELECT s.status, s.changes INTO o FROM sales.sales_order s WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.sales_order_id;
  IF NOT FOUND THEN
    RETURN NEW; -- the foreign key refuses an item of no order
  END IF;
  IF o.status IN ('draft', 'confirmed') AND o.changes = 0 AND NOT EXISTS (SELECT FROM sales.sales_order_change c
      WHERE c.tenant_id = NEW.tenant_id AND c.sales_order_id = NEW.sales_order_id) THEN
    RETURN NEW;
  END IF;
  IF o.status NOT IN ('draft', 'confirmed') OR NOT EXISTS (SELECT FROM sales.sales_order_change c
      WHERE c.tenant_id = NEW.tenant_id AND c.sales_order_id = NEW.sales_order_id AND c.change_no = o.changes + 1) THEN
    RAISE EXCEPTION 'sales order % item %: an item is added to an order that is not cancelled with a kept change', NEW.sales_order_id, NEW.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_added_kept';
  END IF;
  IF NEW.item_no <= (SELECT max(i.item_no) FROM sales.sales_order_item i WHERE i.tenant_id = NEW.tenant_id AND i.sales_order_id = NEW.sales_order_id) THEN
    RAISE EXCEPTION 'sales order % item %: an item added is numbered after the order''s highest', NEW.sales_order_id, NEW.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_added_after';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_sales_order_item_insert() OWNER TO erp_owner;
CREATE TRIGGER sales_order_item_added BEFORE INSERT ON sales.sales_order_item
  FOR EACH ROW EXECUTE FUNCTION sales.check_sales_order_item_insert();

-- An order item's quantity and price change only while the order is not
-- cancelled and nothing of the item is billed (000355); it is removed
-- only in the transaction that keeps the order's next change, naming
-- that change, and a removed item changes no more.
CREATE OR REPLACE FUNCTION sales.check_sales_order_item_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.removed_in_change IS NOT NULL THEN
    RAISE EXCEPTION 'sales order % item %: it is removed, so it changes no more', OLD.sales_order_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_removed_fixed';
  END IF;
  IF OLD.billed_quantity <> 0 THEN
    RAISE EXCEPTION 'sales order % item %: something of it is billed, so its quantity and price stay', OLD.sales_order_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_billed_fixed';
  END IF;
  IF NOT EXISTS (SELECT FROM sales.sales_order o
      WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.sales_order_id AND o.status IN ('draft', 'confirmed')) THEN
    RAISE EXCEPTION 'sales order % item %: the order is cancelled', OLD.sales_order_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_order_open';
  END IF;
  IF NEW.removed_in_change IS NOT NULL AND NOT EXISTS (SELECT FROM sales.sales_order o JOIN sales.sales_order_change c
        ON c.tenant_id = o.tenant_id AND c.sales_order_id = o.id AND c.change_no = o.changes + 1
      WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.sales_order_id AND c.change_no = NEW.removed_in_change) THEN
    RAISE EXCEPTION 'sales order % item %: an item is removed with the order''s next change kept, which it names', OLD.sales_order_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_removed_kept';
  END IF;
  RETURN NEW;
END $$;
DROP TRIGGER sales_order_item_change ON sales.sales_order_item;
CREATE TRIGGER sales_order_item_change
  BEFORE UPDATE OF quantity, unit_price, price_unit, price_source, conditions, net_amount, removed_in_change ON sales.sales_order_item
  FOR EACH ROW EXECUTE FUNCTION sales.check_sales_order_item_change();
GRANT UPDATE (removed_in_change) ON sales.sales_order_item TO erp_app;

-- An order's payment term changes only with a kept change, while no
-- billing document of it stands (one not cancelled reads the order's
-- term); the order's other rules are 000355's.
CREATE FUNCTION sales.check_sales_order_payment_term() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.changes <> OLD.changes + 1 THEN
    RAISE EXCEPTION 'sales order %: its payment term changes with a kept change', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_payment_term_kept';
  END IF;
  IF EXISTS (SELECT FROM sales.billing_document b WHERE b.tenant_id = NEW.tenant_id AND b.sales_order_id = NEW.id AND b.status <> 'cancelled') THEN
    RAISE EXCEPTION 'sales order %: it is billed, so its payment term stays', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_payment_term_billed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_sales_order_payment_term() OWNER TO erp_owner;
CREATE TRIGGER sales_order_payment_term BEFORE UPDATE OF payment_term_code ON sales.sales_order
  FOR EACH ROW WHEN (NEW.payment_term_code IS DISTINCT FROM OLD.payment_term_code)
  EXECUTE FUNCTION sales.check_sales_order_payment_term();
GRANT UPDATE (payment_term_code) ON sales.sales_order TO erp_app;
