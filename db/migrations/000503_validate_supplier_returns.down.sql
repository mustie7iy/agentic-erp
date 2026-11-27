-- A validated check cannot be marked unvalidated again: the down restores
-- 000502's state by adding the three checks back NOT VALID.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '122', '123', '351', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602',
                             '643', '701', '702')) NOT VALID,
  DROP CONSTRAINT material_document_reversal_check,
  ADD CONSTRAINT material_document_reversal_check
    CHECK ((reversal_of_id IS NOT NULL) = (movement_type IN ('102', '123', '502', '562', '202', '552', '312', '302', '602'))) NOT VALID,
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '122', '123', '351', '601', '602', '643'))) NOT VALID;
