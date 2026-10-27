-- 000065's rule of a released order's items, without the assignment.
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
  IF TG_OP = 'UPDATE' AND (NEW.account_id, NEW.uom_code, NEW.material_id, NEW.contract_item_no)
     IS DISTINCT FROM (OLD.account_id, OLD.uom_code, OLD.material_id, OLD.contract_item_no) THEN
    RAISE EXCEPTION 'item % of released purchase order % keeps its account, unit, material and contract item', NEW.item_no, NEW.purchase_order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_identity';
  END IF;
  SELECT coalesce(sum(quantity), 0) INTO received FROM procurement.goods_movement_item
  WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.purchase_order_id AND po_item_no = NEW.item_no;
  IF (NEW.is_removed AND received <> 0) OR NEW.quantity * (1 + NEW.over_delivery_pct / 100) < received THEN
    RAISE EXCEPTION 'item % of purchase order % would be below the % received', NEW.item_no, NEW.purchase_order_id, trim_scale(received)
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_received';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE payables.supplier_invoice_item DROP COLUMN profit_center_id, DROP COLUMN cost_center_id;
ALTER TABLE procurement.purchase_order_item DROP CONSTRAINT purchase_order_item_account_assignment,
  DROP COLUMN cost_center_id, DROP COLUMN account_assignment;
ALTER TABLE procurement.requisition_item DROP CONSTRAINT requisition_item_account_assignment,
  DROP COLUMN cost_center_id, DROP COLUMN account_assignment;
