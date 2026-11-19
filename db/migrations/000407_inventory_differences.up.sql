-- Inventory differences posted (S2d; ADR-0082 As built S2d; 03-data-model
-- §3.35): a counted document's differences are posted as material
-- documents of movement types 701 (gains) and 702 (losses) against the
-- valuation class's new inventory difference account, or wait for the
-- controller's approval when their value exceeds the company's tolerance.
-- A count document keeps the approval request and the value it was asked
-- for and when it was decided; each item its difference, the value posted
-- and the material document that posted it. The material document's
-- movement types and the account determination's keys are widened NOT
-- VALID and validated by 000408 (06 §6.4); the rest is expand only:
-- nullable columns and permissions the previous version neither reads nor
-- writes.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '701', '702')) NOT VALID;
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue')
         OR key ~ '^inventory_(stock|receipt|initial_entry|consumption|scrapping|price_difference|revaluation|difference):[0-9A-Z]{1,4}$') NOT VALID;

-- The approval a document's differences wait for, the value of its
-- differences (gains and losses alike, in its company's currency) it was
-- asked for, and when it was closed: posted, rejected or withdrawn.
ALTER TABLE inventory.count_document
  ADD COLUMN workflow_request_id uuid,           -- workflow context: its approval request
  ADD COLUMN difference_value bigint CONSTRAINT count_document_difference_value_check CHECK (difference_value >= 0),
  ADD COLUMN currency char(3) REFERENCES ref.currency,
  ADD COLUMN closed_at timestamptz,
  -- The value asked for is in the company's currency, which a posted
  -- document's items' values are in too.
  ADD CONSTRAINT count_document_approval CHECK ((workflow_request_id IS NULL) = (difference_value IS NULL)
    AND (difference_value IS NULL OR currency IS NOT NULL) AND (status <> 'posted' OR currency IS NOT NULL)),
  ADD CONSTRAINT count_document_closed CHECK ((closed_at IS NOT NULL) = (status IN ('posted', 'rejected', 'withdrawn')));
GRANT UPDATE (workflow_request_id, difference_value, currency, closed_at) ON inventory.count_document TO erp_app;

-- An item posted: its difference (counted less book) and the value it
-- moved, and the material document that moved it when it was not zero.
ALTER TABLE inventory.count_item
  ADD COLUMN difference numeric(20,6),
  ADD COLUMN amount bigint,
  ADD COLUMN material_document_id uuid,
  ADD CONSTRAINT count_item_material_document_fkey FOREIGN KEY (tenant_id, material_document_id) REFERENCES inventory.material_document (tenant_id, id),
  ADD CONSTRAINT count_item_posted CHECK ((difference IS NULL) = (amount IS NULL)
    AND (difference IS NULL OR counted_quantity IS NOT NULL) AND (material_document_id IS NULL OR (difference <> 0 AND NOT open))
    AND (difference IS NULL OR difference = 0 OR material_document_id IS NOT NULL));
GRANT UPDATE (difference, amount, material_document_id) ON inventory.count_item TO erp_app;
-- An item is counted once, closed once and posted once; what it counts
-- and its book quantity never change.
CREATE OR REPLACE FUNCTION inventory.count_item_once() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.counted_quantity IS NOT NULL AND (NEW.counted_quantity, NEW.entry_quantity, NEW.entry_uom_code, NEW.counted_at, NEW.counted_by)
        IS DISTINCT FROM (OLD.counted_quantity, OLD.entry_quantity, OLD.entry_uom_code, OLD.counted_at, OLD.counted_by))
      OR (NOT OLD.open AND NEW.open)
      OR (OLD.difference IS NOT NULL AND (NEW.difference, NEW.amount, NEW.material_document_id)
        IS DISTINCT FROM (OLD.difference, OLD.amount, OLD.material_document_id))
      OR (NEW.material_id, NEW.plant_id, NEW.storage_location_id, NEW.book_quantity, NEW.item_no, NEW.count_document_id)
        IS DISTINCT FROM (OLD.material_id, OLD.plant_id, OLD.storage_location_id, OLD.book_quantity, OLD.item_no, OLD.count_document_id) THEN
    RAISE EXCEPTION 'count_item_once: count document % item %: a count is entered once and an item closed and posted once', OLD.count_document_id,
      OLD.item_no USING ERRCODE = 'check_violation', CONSTRAINT = 'count_item_once';
  END IF;
  RETURN NEW;
END $$;

-- Posting a count's differences, and approving them above the company's
-- tolerance, an approval permission agents never hold (ADR-0082 As built
-- S2d).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('inventory.count.post', 'Post the differences of counted physical inventory documents, which moves stock and its value', false),
  ('inventory.difference.approve', 'Approve inventory differences above the company''s tolerance before they are posted', true);
