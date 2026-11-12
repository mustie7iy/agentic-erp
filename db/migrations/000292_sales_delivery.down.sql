DELETE FROM identity.sod_rule WHERE permission_a = 'sales.delivery.confirm' OR permission_b = 'sales.delivery.confirm';
DELETE FROM identity.role_permission WHERE permission_code = 'sales.delivery.confirm';
DELETE FROM ref.permission WHERE code = 'sales.delivery.confirm';
DROP TABLE sales.delivery_item;
DROP TABLE sales.delivery;
DROP FUNCTION sales.record_delivery_item();
DROP FUNCTION sales.record_delivery();
-- The order's change rule as 000291 made it. Quantities delivered stay on
-- the order's items, which nothing changes again.
CREATE OR REPLACE FUNCTION sales.check_sales_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' OR NEW.status <> 'confirmed' THEN
    RAISE EXCEPTION 'sales order %: only a draft changes, and only by being confirmed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sales_order_confirmed_once';
  END IF;
  RETURN NEW;
END $$;
