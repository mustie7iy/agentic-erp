-- Drops what 000477 added: an item's confirmed quantity no longer says
-- what part of it rests on the lead time, as the previous version reads it.
REVOKE UPDATE (lead_time_quantity, lead_time_date) ON sales.sales_order_item FROM erp_app;
ALTER TABLE sales.sales_order_item
  DROP CONSTRAINT sales_order_item_lead_time,
  DROP COLUMN lead_time_date,
  DROP COLUMN lead_time_quantity;
