DELETE FROM identity.sod_rule WHERE permission_a IN ('sales.order.read', 'sales.order.write', 'sales.order.confirm')
  OR permission_b IN ('sales.order.read', 'sales.order.write', 'sales.order.confirm');
DELETE FROM identity.role_permission WHERE permission_code IN ('sales.order.read', 'sales.order.write', 'sales.order.confirm');
DELETE FROM ref.permission WHERE code IN ('sales.order.read', 'sales.order.write', 'sales.order.confirm');
DROP TABLE sales.sales_order_item;
DROP TABLE sales.sales_order;
DROP FUNCTION sales.check_sales_order_change();
DROP FUNCTION sales.check_sales_order_quotation();
