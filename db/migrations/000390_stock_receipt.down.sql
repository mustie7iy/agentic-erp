-- The revert is refused while a goods receipt moved stock or an invoice
-- put a difference on it: the previous version neither moves nor values
-- them, and what they posted is not undone silently.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.material_document WHERE movement_type IN ('101', '102')) OR EXISTS (SELECT FROM inventory.invoice_difference)
     OR EXISTS (SELECT FROM procurement.purchase_order_item WHERE storage_location_id IS NOT NULL) THEN
    RAISE EXCEPTION 'stock items, goods receipts into stock or invoice differences on stock exist: reverting would drop them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE inventory.invoice_difference;
DROP FUNCTION inventory.invoice_difference_valuation();
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_reference,
  DROP COLUMN reference,
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302')),
  DROP CONSTRAINT material_document_reversal_check,
  ADD CONSTRAINT material_document_reversal_check
    CHECK ((reversal_of_id IS NOT NULL) = (movement_type IN ('502', '562', '202', '552', '312', '302')));
ALTER TABLE procurement.goods_movement DROP CONSTRAINT goods_movement_material_document, DROP COLUMN material_document_id,
  DROP COLUMN material_document_number;
CREATE OR REPLACE FUNCTION procurement.check_order_item_editable() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text; applying uuid; received numeric;
BEGIN
  SELECT status, applying_change_id INTO st, applying FROM procurement.purchase_order
  WHERE tenant_id = NEW.tenant_id AND id = NEW.purchase_order_id;
  IF st = 'draft' THEN
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE' AND st = 'pending_approval' AND OLD.valuation_price IS NULL AND NEW.valuation_price IS NOT NULL
     AND to_jsonb(NEW) - 'valuation_price' - 'valuation_price_unit' = to_jsonb(OLD) - 'valuation_price' - 'valuation_price_unit' THEN
    RETURN NEW;
  END IF;
  IF st <> 'released' OR applying IS NULL THEN
    RAISE EXCEPTION 'items of a % procurement.purchase_order cannot change', st USING ERRCODE = 'check_violation';
  END IF;
  IF NOT NEW.is_removed AND NEW.valuation_price IS NULL THEN
    RAISE EXCEPTION 'item % is added to released purchase order % with its valuation price', NEW.item_no, NEW.purchase_order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_valued';
  END IF;
  IF TG_OP = 'UPDATE' AND (NEW.account_id, NEW.uom_code, NEW.material_id, NEW.contract_item_no, NEW.account_assignment, NEW.cost_center_id)
     IS DISTINCT FROM (OLD.account_id, OLD.uom_code, OLD.material_id, OLD.contract_item_no, OLD.account_assignment, OLD.cost_center_id) THEN
    RAISE EXCEPTION 'item % of released purchase order % keeps its account, assignment, unit, material and contract item', NEW.item_no,
      NEW.purchase_order_id USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_identity';
  END IF;
  SELECT coalesce(sum(quantity), 0) INTO received FROM procurement.goods_movement_item
  WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.purchase_order_id AND po_item_no = NEW.item_no;
  IF (NEW.is_removed AND received <> 0) OR NEW.quantity * (1 + NEW.over_delivery_pct / 100) < received THEN
    RAISE EXCEPTION 'item % of purchase order % would be below the % received', NEW.item_no, NEW.purchase_order_id, trim_scale(received)
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_received';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE procurement.purchase_order_item DROP CONSTRAINT purchase_order_item_stock, DROP COLUMN plant_id, DROP COLUMN storage_location_id;
