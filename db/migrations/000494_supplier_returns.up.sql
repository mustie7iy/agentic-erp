-- Returns to the supplier of stock items (S1e2; ADR-0082 As built S1e2;
-- 03-data-model §3.35). A return (122) of a purchase order's stock item,
-- which procurement's goods movement posts, takes the goods out of stock
-- as a material document of movement type 122, and its reversal (123)
-- puts them back, naming the 122; both name the goods movement's number
-- as their reference, as a goods receipt's 101 and 102 do. Expand only
-- (06 §6.4): the checks widened NOT VALID (validated by 000495), which
-- the previous version, writing neither type, always passes. The
-- reversal trigger (000376's material_document_reversal) already pairs a
-- 123 with a 122 of its company.
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
