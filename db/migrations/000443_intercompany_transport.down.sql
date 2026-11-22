-- Intercompany orders and goods issues the previous version cannot hold
-- are refused rather than dropped (06 §6.4: a down migration loses no
-- data).
DO $$
BEGIN
  IF EXISTS (SELECT FROM procurement.purchase_order WHERE supplying_plant_id IS NOT NULL)
     OR EXISTS (SELECT FROM inventory.material_document WHERE movement_type = '643') THEN
    RAISE EXCEPTION 'intercompany stock transport orders exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
-- The checks go back to 000419's state, validated.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '351', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602', '701',
                             '702')),
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '351', '601', '602')));
DROP TRIGGER purchase_order_supply_fixed ON procurement.purchase_order;
DROP FUNCTION procurement.purchase_order_supply_fixed();
ALTER TABLE procurement.purchase_order
  DROP CONSTRAINT purchase_order_issue,
  DROP COLUMN issue_date,
  DROP COLUMN issue_document_number,
  DROP COLUMN issue_document_id,
  DROP COLUMN supplying_plant_id;
