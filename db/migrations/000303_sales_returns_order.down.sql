DELETE FROM identity.sod_rule WHERE permission_a IN ('sales.return.read', 'sales.return.write', 'sales.return.confirm')
  OR permission_b IN ('sales.return.read', 'sales.return.write', 'sales.return.confirm');
DELETE FROM identity.role_permission WHERE permission_code IN ('sales.return.read', 'sales.return.write', 'sales.return.confirm');
DELETE FROM ref.permission WHERE code IN ('sales.return.read', 'sales.return.write', 'sales.return.confirm');
DROP TABLE sales.returns_order_item;
DROP TABLE sales.returns_order;
DROP FUNCTION sales.record_returns_order_item();
DROP FUNCTION sales.check_returns_order_change();
DROP FUNCTION sales.record_returns_order();
-- What was returned leaves with the returns orders.
ALTER TABLE sales.sales_order_item
  DROP CONSTRAINT sales_order_item_returned,
  DROP COLUMN returned_quantity;
