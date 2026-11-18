DROP TABLE sales.sales_order_change;
REVOKE UPDATE (quantity, unit_price, price_unit, price_source, conditions, net_amount) ON sales.sales_order_item FROM erp_app;
DROP TRIGGER sales_order_item_change ON sales.sales_order_item;
DROP FUNCTION sales.check_sales_order_item_change();
REVOKE UPDATE (requested_delivery_date, net_amount, changes, cancellation_reason, cancelled_at, cancelled_by, cancelled_by_name) ON sales.sales_order FROM erp_app;
-- The change rule as 000300 made it.
CREATE OR REPLACE FUNCTION sales.check_sales_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'confirmed' AND NEW.status = 'confirmed' AND NEW.credit_block = OLD.credit_block AND pg_trigger_depth() > 1 THEN
    RETURN NEW;
  END IF;
  IF OLD.status = 'confirmed' AND NEW.status = 'confirmed' AND OLD.credit_block = 'blocked' AND NEW.credit_block = 'released' THEN
    RETURN NEW;
  END IF;
  IF OLD.status <> 'draft' OR NEW.status <> 'confirmed' OR NEW.credit_block = 'released' THEN
    RAISE EXCEPTION 'sales order %: only a draft changes, and only by being confirmed; a credit block is released once', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
-- The status and credit block checks restored refuse the revert while a
-- cancelled order exists: the previous version knows none. A changed
-- order keeps its items as they were changed.
ALTER TABLE sales.sales_order
  DROP CONSTRAINT sales_order_credit_block_confirmed,
  ADD CONSTRAINT sales_order_credit_block_confirmed CHECK (status = 'confirmed' OR credit_block = 'none'),
  DROP CONSTRAINT sales_order_requested_delivery,
  DROP CONSTRAINT sales_order_cancelled,
  DROP CONSTRAINT sales_order_status_check,
  ADD CONSTRAINT sales_order_status_check CHECK (status IN ('draft', 'confirmed')),
  DROP COLUMN cancelled_by_name,
  DROP COLUMN cancelled_by,
  DROP COLUMN cancelled_at,
  DROP COLUMN cancellation_reason,
  DROP COLUMN changes,
  DROP COLUMN requested_delivery_date;
