DELETE FROM identity.sod_rule WHERE permission_a IN ('inventory.count.post', 'inventory.difference.approve')
  OR permission_b IN ('inventory.count.post', 'inventory.difference.approve');
DELETE FROM identity.role_permission WHERE permission_code IN ('inventory.count.post', 'inventory.difference.approve');
DELETE FROM ref.permission WHERE code IN ('inventory.count.post', 'inventory.difference.approve');
CREATE OR REPLACE FUNCTION inventory.count_item_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.counted_quantity IS NOT NULL AND (NEW.counted_quantity, NEW.entry_quantity, NEW.entry_uom_code, NEW.counted_at, NEW.counted_by)
        IS DISTINCT FROM (OLD.counted_quantity, OLD.entry_quantity, OLD.entry_uom_code, OLD.counted_at, OLD.counted_by))
      OR (NOT OLD.open AND NEW.open)
      OR (NEW.material_id, NEW.plant_id, NEW.storage_location_id, NEW.book_quantity, NEW.item_no, NEW.count_document_id)
        IS DISTINCT FROM (OLD.material_id, OLD.plant_id, OLD.storage_location_id, OLD.book_quantity, OLD.item_no, OLD.count_document_id) THEN
    RAISE EXCEPTION 'count_item_once: count document % item %: a count is entered once and an item closed once', OLD.count_document_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'count_item_once';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE inventory.count_item DROP CONSTRAINT count_item_posted, DROP CONSTRAINT count_item_material_document_fkey,
  DROP COLUMN material_document_id, DROP COLUMN amount, DROP COLUMN difference;
ALTER TABLE inventory.count_document DROP CONSTRAINT count_document_closed, DROP CONSTRAINT count_document_approval,
  DROP COLUMN closed_at, DROP COLUMN currency, DROP COLUMN difference_value, DROP COLUMN workflow_request_id;
DELETE FROM ledger.account_determination WHERE key LIKE 'inventory_difference:%';
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue')
         OR key ~ '^inventory_(stock|receipt|initial_entry|consumption|scrapping|price_difference|revaluation):[0-9A-Z]{1,4}$') NOT VALID;
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302')) NOT VALID;
