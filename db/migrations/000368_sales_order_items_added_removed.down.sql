-- The revert is refused while an order has an item removed, an item
-- added or its payment term changed by a change: the previous version
-- would count a removed item in its order's value again, and knows no
-- added item or term in the changes it lists.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM sales.sales_order_item WHERE removed_in_change IS NOT NULL)
      OR EXISTS (SELECT FROM sales.sales_order_change WHERE payment_term_code_before IS NOT NULL
        OR jsonb_path_exists(items, '$[*] ? (@.change == "added" || @.change == "removed")')) THEN
    RAISE EXCEPTION 'sales orders hold items added or removed, or a payment term changed, by a change: reverting would drop them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
REVOKE UPDATE (payment_term_code) ON sales.sales_order FROM erp_app;
DROP TRIGGER sales_order_payment_term ON sales.sales_order;
DROP FUNCTION sales.check_sales_order_payment_term();
REVOKE UPDATE (removed_in_change) ON sales.sales_order_item FROM erp_app;
DROP TRIGGER sales_order_item_change ON sales.sales_order_item;
CREATE OR REPLACE FUNCTION sales.check_sales_order_item_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.billed_quantity <> 0 THEN
    RAISE EXCEPTION 'sales order % item %: something of it is billed, so its quantity and price stay', OLD.sales_order_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_billed_fixed';
  END IF;
  IF NOT EXISTS (SELECT FROM sales.sales_order o
      WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.sales_order_id AND o.status IN ('draft', 'confirmed')) THEN
    RAISE EXCEPTION 'sales order % item %: the order is cancelled', OLD.sales_order_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_item_order_open';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER sales_order_item_change
  BEFORE UPDATE OF quantity, unit_price, price_unit, price_source, conditions, net_amount ON sales.sales_order_item
  FOR EACH ROW EXECUTE FUNCTION sales.check_sales_order_item_change();
DROP TRIGGER sales_order_item_added ON sales.sales_order_item;
DROP FUNCTION sales.check_sales_order_item_insert();
ALTER TABLE sales.sales_order_change
  DROP CONSTRAINT sales_order_change_payment_term,
  DROP COLUMN payment_term_code_after,
  DROP COLUMN payment_term_code_before;
ALTER TABLE sales.sales_order_item
  DROP CONSTRAINT sales_order_item_removed_undelivered,
  DROP COLUMN removed_in_change;
