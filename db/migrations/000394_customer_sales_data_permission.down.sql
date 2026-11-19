-- Back to sales.price.read and .write on the customer sales data: the
-- previous release reads those, so dropping the new permissions, their
-- grants and any segregation-of-duties rule naming them takes no one's
-- access away that the old permissions do not give. Row-level security
-- is off so every tenant's rows go (or the migration fails).
SET LOCAL row_security = off;
DELETE FROM identity.sod_rule WHERE permission_a IN ('sales.customer_sales_data.read', 'sales.customer_sales_data.write')
  OR permission_b IN ('sales.customer_sales_data.read', 'sales.customer_sales_data.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('sales.customer_sales_data.read', 'sales.customer_sales_data.write');
DELETE FROM ref.permission WHERE code IN ('sales.customer_sales_data.read', 'sales.customer_sales_data.write');
