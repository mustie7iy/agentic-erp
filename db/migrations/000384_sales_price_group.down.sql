SET LOCAL row_security = off; -- the down migration deletes every tenant's group records (06 §6.4)
-- The previous version would read a price group's record as a general
-- one, for every customer, so the group records go with the column.
ALTER TABLE sales.price DISABLE TRIGGER price_immutable;
ALTER TABLE sales.price_scale DISABLE TRIGGER price_scale_immutable;
DELETE FROM sales.price_scale s USING sales.price p WHERE s.tenant_id = p.tenant_id AND s.price_id = p.id AND p.price_group IS NOT NULL;
DELETE FROM sales.price WHERE price_group IS NOT NULL;
ALTER TABLE sales.price_scale ENABLE TRIGGER price_scale_immutable;
ALTER TABLE sales.price ENABLE TRIGGER price_immutable;
ALTER TABLE sales.price DROP CONSTRAINT price_customer_or_group, DROP COLUMN price_group;
DROP TABLE sales.customer_sales_data;
