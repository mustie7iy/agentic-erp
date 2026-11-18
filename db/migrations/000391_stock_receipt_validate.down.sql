-- A validated check cannot be marked unvalidated again: the down restores
-- 000390's state by adding the checks back NOT VALID.
ALTER TABLE procurement.purchase_order_item DROP CONSTRAINT purchase_order_item_stock,
  ADD CONSTRAINT purchase_order_item_stock CHECK ((plant_id IS NULL) = (storage_location_id IS NULL)
    AND (storage_location_id IS NULL OR (material_id IS NOT NULL AND account_assignment = 'none'))) NOT VALID;
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302')) NOT VALID,
  DROP CONSTRAINT material_document_reversal_check,
  ADD CONSTRAINT material_document_reversal_check
    CHECK ((reversal_of_id IS NOT NULL) = (movement_type IN ('102', '502', '562', '202', '552', '312', '302'))) NOT VALID,
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102'))) NOT VALID;
