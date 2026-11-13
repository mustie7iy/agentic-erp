DELETE FROM identity.sod_rule WHERE permission_a = 'sales.order.credit_release' OR permission_b = 'sales.order.credit_release';
DELETE FROM identity.role_permission WHERE permission_code = 'sales.order.credit_release';
DELETE FROM ref.permission WHERE code = 'sales.order.credit_release';
-- The delivery's rule and the order's change rule as 000292 made them. An
-- order that was blocked reads as confirmed again, as every confirmed
-- order did before the credit block.
CREATE OR REPLACE FUNCTION sales.record_delivery() RETURNS trigger
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
REVOKE UPDATE (credit_block, credit_released_by, credit_released_by_name, credit_released_at) ON sales.sales_order FROM erp_app;
ALTER TABLE sales.sales_order
  DROP CONSTRAINT sales_order_credit_block_confirmed,
  DROP CONSTRAINT sales_order_credit_release,
  DROP COLUMN credit_released_at,
  DROP COLUMN credit_released_by_name,
  DROP COLUMN credit_released_by,
  DROP COLUMN credit_block;
