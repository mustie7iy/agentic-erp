-- Sales items by material (S1f1; ADR-0082 As built S1f; 03-data-model
-- §3.30). A quotation's or a sales order's item may name a material of the
-- tenant; its item key is then the material's number, so the price
-- records and everything else that reads items by key find it. The
-- material is the materials context's, by id: no foreign key leaves a
-- schema but ref's (02 §2.1). An order item keeps the material it was
-- entered with: the application's column grants (000355) do not include
-- material_id, and a quotation's items are immutable (000290). Expand only
-- (06 §6.4): a nullable column the previous version neither reads nor
-- writes, and a check added NOT VALID (validated by 000396) that the
-- previous version, which writes no material, always passes.
ALTER TABLE sales.quotation_item
  ADD COLUMN material_id uuid,  -- materials context
  ADD CONSTRAINT quotation_item_material_key CHECK (material_id IS NULL OR item_key IS NOT NULL) NOT VALID;
ALTER TABLE sales.sales_order_item
  ADD COLUMN material_id uuid,  -- materials context
  ADD CONSTRAINT sales_order_item_material_key CHECK (material_id IS NULL OR item_key IS NOT NULL) NOT VALID;
