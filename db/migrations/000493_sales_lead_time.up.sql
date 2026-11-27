-- What a confirmation rests on the replenishment lead time (S2j2; ADR-0082
-- As built S2j2; 03-data-model §3.16): an order item's confirmed quantity
-- may include a part the plant's stock did not cover when the
-- availability check confirmed it, which the material's replenishment
-- lead time at the plant (000492) covers by lead_time_date. Both are set
-- together, only beside a confirmed quantity, the part more than zero and
-- at most what is confirmed; the check writes them with the confirmed
-- quantity, whose trigger (000430) keeps that to confirmed orders with a
-- plant. Expand only (06 §6.4): nullable columns, null on every existing
-- item, which the previous version neither reads nor writes. One write
-- of such a server is refused during the rollout: a change that lowers,
-- below its part on the lead time, what an item a new server confirmed
-- on the lead time confirms meets sales_order_item_lead_time (a 500
-- there, where the new server writes both).
ALTER TABLE sales.sales_order_item
  ADD COLUMN lead_time_quantity numeric(20,6),
  ADD COLUMN lead_time_date date,
  ADD CONSTRAINT sales_order_item_lead_time CHECK ((lead_time_quantity IS NULL) = (lead_time_date IS NULL)
    AND (lead_time_quantity IS NULL OR (confirmed_quantity IS NOT NULL AND lead_time_quantity > 0
      AND lead_time_quantity <= confirmed_quantity)));
GRANT UPDATE (lead_time_quantity, lead_time_date) ON sales.sales_order_item TO erp_app;
