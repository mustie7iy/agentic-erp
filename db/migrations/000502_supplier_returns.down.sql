-- Returns to the supplier of stock items the previous version cannot hold
-- are refused rather than dropped (06 §6.4: a down migration loses no
-- data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.material_document WHERE movement_type IN ('122', '123')) THEN
    RAISE EXCEPTION 'returns of stock items to the supplier exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
-- The checks go back to 000444's state, validated.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '351', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602', '643',
                             '701', '702')),
  DROP CONSTRAINT material_document_reversal_check,
  ADD CONSTRAINT material_document_reversal_check
    CHECK ((reversal_of_id IS NOT NULL) = (movement_type IN ('102', '502', '562', '202', '552', '312', '302', '602'))),
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '351', '601', '602', '643')));
