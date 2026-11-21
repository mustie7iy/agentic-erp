-- An order's plant, its items' confirmed quantities and the plant of a
-- delivery assignment go: the previous version delivers what is ordered
-- and open, whatever was confirmed.
DROP INDEX sales.sales_order_item_promised;
DROP INDEX sales.sales_order_item_in_delivery_held;
DROP INDEX sales.sales_order_by_plant;
DROP INDEX sales.delivery_assignment_by_plant;
ALTER TABLE sales.delivery_assignment
  DROP COLUMN plant_id;
REVOKE UPDATE (confirmed_quantity) ON sales.sales_order_item FROM erp_app;
DROP TRIGGER sales_order_item_confirmed_by_check ON sales.sales_order_item;
DROP FUNCTION sales.check_sales_order_item_confirmed();
ALTER TABLE sales.sales_order_item
  DROP CONSTRAINT sales_order_item_confirmed,
  DROP COLUMN confirmed_quantity;
ALTER TABLE sales.sales_order
  DROP COLUMN plant_id;
