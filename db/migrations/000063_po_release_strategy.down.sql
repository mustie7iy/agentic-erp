DROP TRIGGER purchase_order_transition ON procurement.purchase_order;
DROP FUNCTION procurement.check_order_transition();
CREATE OR REPLACE FUNCTION procurement.value_released_items() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status = 'released' AND OLD.status <> 'released' THEN
    UPDATE procurement.purchase_order_item SET valuation_price = unit_price, valuation_price_unit = price_unit
    WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.id AND NOT is_removed AND valuation_price IS NULL;
  END IF;
  RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION procurement.check_valuation_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.valuation IS DISTINCT FROM OLD.valuation AND NOT (OLD.status = 'draft' AND NEW.status = 'released') THEN
    RAISE EXCEPTION 'the valuation of purchase order % is fixed when it is released', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_valuation_fixed';
  END IF;
  RETURN NEW;
END $$;
-- Orders waiting for approval go back to draft; their requests are the
-- workflow context's to end.
SET LOCAL row_security = off;
UPDATE procurement.purchase_order SET status = 'draft', release_request_id = NULL WHERE status = 'pending_approval';
ALTER TABLE procurement.purchase_order DROP CONSTRAINT purchase_order_draft_direct,
  ADD CONSTRAINT purchase_order_draft_direct CHECK (status <> 'draft' OR valuation = 'direct'),
  DROP CONSTRAINT purchase_order_release_request_pending, DROP COLUMN release_request_id,
  DROP CONSTRAINT purchase_order_status_check,
  ADD CONSTRAINT purchase_order_status_check CHECK (status IN ('draft', 'released', 'closed', 'cancelled'));
