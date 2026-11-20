-- An order's plant and its items' confirmed quantities go: the previous
-- version delivers what is ordered and open, whatever was confirmed.
DROP INDEX sales.sales_order_item_promised;
DROP INDEX sales.sales_order_by_plant;
REVOKE UPDATE (confirmed_quantity) ON sales.sales_order_item FROM erp_app;
DROP TRIGGER sales_order_item_confirmed_by_check ON sales.sales_order_item;
DROP FUNCTION sales.check_sales_order_item_confirmed();
ALTER TABLE sales.sales_order_item
  DROP CONSTRAINT sales_order_item_confirmed,
  DROP COLUMN confirmed_quantity;
ALTER TABLE sales.sales_order
  DROP COLUMN plant_id;
