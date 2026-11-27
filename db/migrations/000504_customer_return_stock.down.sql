-- Customer returns received into stock, which the previous version cannot
-- hold, are refused rather than dropped (06 §6.4: a down migration loses
-- no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM sales.return_receipt_posting) OR EXISTS (SELECT FROM inventory.material_document WHERE movement_type = '653') THEN
    RAISE EXCEPTION 'customer returns received into stock exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE sales.return_receipt_item_cost;
DROP TABLE sales.return_receipt_posting;
-- The checks go back to 000503's state (S1e2's, 122 and 123 kept),
-- validated.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '122', '123', '351', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602',
                             '643', '701', '702')),
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '122', '123', '351', '601', '602', '643')));
