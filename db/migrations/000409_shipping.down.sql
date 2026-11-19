DELETE FROM identity.sod_rule WHERE permission_a IN ('shipping.delivery.read', 'shipping.delivery.write')
  OR permission_b IN ('shipping.delivery.read', 'shipping.delivery.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('shipping.delivery.read', 'shipping.delivery.write');
DELETE FROM ref.permission WHERE code IN ('shipping.delivery.read', 'shipping.delivery.write');
DROP TRIGGER sales_order_cancelled_unshipped ON sales.sales_order;
DROP FUNCTION sales.check_sales_order_unshipped();
DROP TABLE sales.delivery_assignment;
DROP FUNCTION sales.record_delivery_assignment();
ALTER TABLE sales.sales_order_item
  DROP CONSTRAINT sales_order_item_removed_unshipped,
  DROP CONSTRAINT sales_order_item_in_delivery,
  DROP COLUMN in_delivery_quantity;
REVOKE ALL ON SCHEMA shipping FROM erp_relay;
DROP SCHEMA shipping CASCADE;
