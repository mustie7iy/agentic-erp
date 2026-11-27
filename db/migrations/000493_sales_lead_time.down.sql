-- Order items confirmed in part on the lead time, which the previous
-- version cannot hold, are refused rather than dropped (06 §6.4: a down
-- migration loses no data; batch 96S integration). Otherwise the down
-- drops what 000493 added: an item's confirmed quantity no longer says
-- what part of it rests on the lead time, as the previous version reads it.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM sales.sales_order_item WHERE lead_time_quantity IS NOT NULL) THEN
    RAISE EXCEPTION 'sales order items are confirmed in part on the lead time; the previous version cannot hold it' USING ERRCODE = 'check_violation';
  END IF;
END $$;
REVOKE UPDATE (lead_time_quantity, lead_time_date) ON sales.sales_order_item FROM erp_app;
ALTER TABLE sales.sales_order_item
  DROP CONSTRAINT sales_order_item_lead_time,
  DROP COLUMN lead_time_date,
  DROP COLUMN lead_time_quantity;
