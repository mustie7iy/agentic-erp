-- Account assignment on requisition, purchase order and supplier invoice
-- items (T56e, ADR-0050 §6). An order item's account_assignment (SAP's
-- account assignment category: none, or K for a cost center) and cost
-- center; an invoice item's cost center and profit center. The centers are
-- the ledger's (another context): checked through its published API when
-- the document is released or posted, with no foreign key across schemas
-- (ADR-0050 §2), as the items' account_id. Expand only (06 §6.4): the
-- columns take a constant default or none, rewriting nothing, and the
-- rule tying the two is added NOT VALID, reading none of the rows under
-- the lock; 000162 validates it. The previous release writes items without
-- the columns, which their defaults satisfy.
ALTER TABLE procurement.requisition_item
  ADD COLUMN account_assignment text NOT NULL DEFAULT 'none',
  ADD COLUMN cost_center_id uuid,
  ADD CONSTRAINT requisition_item_account_assignment
    CHECK (account_assignment IN ('none', 'cost_center') AND (account_assignment = 'cost_center') = (cost_center_id IS NOT NULL)) NOT VALID;
ALTER TABLE procurement.purchase_order_item
  ADD COLUMN account_assignment text NOT NULL DEFAULT 'none',
  ADD COLUMN cost_center_id uuid,
  ADD CONSTRAINT purchase_order_item_account_assignment
    CHECK (account_assignment IN ('none', 'cost_center') AND (account_assignment = 'cost_center') = (cost_center_id IS NOT NULL)) NOT VALID;
ALTER TABLE payables.supplier_invoice_item
  ADD COLUMN cost_center_id uuid,
  ADD COLUMN profit_center_id uuid;

-- A released order item keeps its assignment as it keeps its account
-- (000065): its receipts posted to the cost center, and a receipt's
-- reversal (102), a return (122) and the invoice's price difference post
-- to it after them, so a later change of the center would split the
-- item's postings between two centers. 000065's rule otherwise unchanged.
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
