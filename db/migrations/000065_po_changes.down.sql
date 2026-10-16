DROP TRIGGER purchase_order_item_editable ON procurement.purchase_order_item;
CREATE OR REPLACE FUNCTION procurement.check_order_item_editable() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text;
BEGIN
  SELECT status INTO st FROM procurement.purchase_order WHERE tenant_id = NEW.tenant_id AND id = NEW.purchase_order_id;
  IF st = 'draft' OR (st = 'pending_approval' AND OLD.valuation_price IS NULL AND NEW.valuation_price IS NOT NULL
      AND to_jsonb(NEW) - 'valuation_price' - 'valuation_price_unit' = to_jsonb(OLD) - 'valuation_price' - 'valuation_price_unit') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'items of a % procurement.purchase_order cannot change', st USING ERRCODE = 'check_violation';
END $$;
CREATE TRIGGER purchase_order_item_editable BEFORE UPDATE ON procurement.purchase_order_item
  FOR EACH ROW EXECUTE FUNCTION procurement.check_order_item_editable();
DROP TRIGGER purchase_order_applied_at_once ON procurement.purchase_order;
DROP FUNCTION procurement.check_order_applied();
DROP TRIGGER purchase_order_revision ON procurement.purchase_order;
DROP FUNCTION procurement.check_order_revision();
ALTER TABLE procurement.purchase_order DROP CONSTRAINT purchase_order_applying_change;
DROP TABLE procurement.purchase_order_change;
DROP FUNCTION procurement.check_order_change();
ALTER TABLE procurement.purchase_order DROP CONSTRAINT purchase_order_applying_released,
  DROP COLUMN applying_change_id, DROP COLUMN revision;
