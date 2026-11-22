-- A validated check cannot be marked unvalidated again: the down restores
-- 000437's state by adding both checks back NOT VALID.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '351', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602', '643',
                             '701', '702')) NOT VALID,
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '351', '601', '602', '643'))) NOT VALID;
