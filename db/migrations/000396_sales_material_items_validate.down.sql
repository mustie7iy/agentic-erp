-- A validated check cannot be marked unvalidated again: the down restores
-- 000395's state by adding the checks back NOT VALID.
ALTER TABLE sales.quotation_item DROP CONSTRAINT quotation_item_material_key,
  ADD CONSTRAINT quotation_item_material_key CHECK (material_id IS NULL OR item_key IS NOT NULL) NOT VALID;
ALTER TABLE sales.sales_order_item DROP CONSTRAINT sales_order_item_material_key,
  ADD CONSTRAINT sales_order_item_material_key CHECK (material_id IS NULL OR item_key IS NOT NULL) NOT VALID;
