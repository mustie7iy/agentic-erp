DELETE FROM identity.sod_rule WHERE permission_a IN ('sales.price.read', 'sales.price.write')
  OR permission_b IN ('sales.price.read', 'sales.price.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('sales.price.read', 'sales.price.write');
DELETE FROM ref.permission WHERE code IN ('sales.price.read', 'sales.price.write');
ALTER TABLE sales.sales_order_item DROP COLUMN price_source;
ALTER TABLE sales.quotation_item DROP COLUMN price_source;
DROP TABLE sales.price;
