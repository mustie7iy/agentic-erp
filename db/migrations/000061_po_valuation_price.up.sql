-- Valuation prices (T47a, 03-data-model §3.8). An order item's
-- goods movements are valued through GR/IR at the price it was released
-- at, kept on the item: the GR/IR engine values an order item's whole open
-- position at the price a movement gives it (T33), so a later change of
-- the item's price (an order change, T47f) must not reach its movements;
-- the new price reaches GR/IR only through the invoice's price difference.
-- An item is valued when it is released, with its order or as a change
-- adds it, and its valuation price never changes after that.
ALTER TABLE procurement.purchase_order_item
  ADD COLUMN valuation_price bigint CHECK (valuation_price >= 0),
  ADD COLUMN valuation_price_unit integer CHECK (valuation_price_unit > 0),
  ADD CONSTRAINT purchase_order_item_valuation_pair CHECK ((valuation_price IS NULL) = (valuation_price_unit IS NULL));

-- Items of orders released before T47a are valued at their price, which
-- could not change after the release; every tenant's (06 §6.3).
SET LOCAL row_security = off;
ALTER TABLE procurement.purchase_order_item DISABLE TRIGGER purchase_order_item_editable;
UPDATE procurement.purchase_order_item i SET valuation_price = i.unit_price, valuation_price_unit = i.price_unit
FROM procurement.purchase_order o
WHERE o.tenant_id = i.tenant_id AND o.id = i.purchase_order_id AND o.status <> 'draft' AND NOT i.is_removed;
ALTER TABLE procurement.purchase_order_item ENABLE TRIGGER purchase_order_item_editable;

CREATE FUNCTION procurement.check_valuation_price_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.valuation_price IS NOT NULL AND (NEW.valuation_price, NEW.valuation_price_unit)
     IS DISTINCT FROM (OLD.valuation_price, OLD.valuation_price_unit) THEN
    RAISE EXCEPTION 'item % of purchase order % is valued at the price it was released at', OLD.item_no, OLD.purchase_order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_valuation_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_valuation_price_fixed() OWNER TO erp_owner;
CREATE TRIGGER purchase_order_item_valuation_fixed BEFORE UPDATE OF valuation_price, valuation_price_unit
  ON procurement.purchase_order_item FOR EACH ROW EXECUTE FUNCTION procurement.check_valuation_price_fixed();

-- A released order's items are valued. The release values those not yet
-- valued at their price, as the order is still a draft whose items may
-- change: the application values them itself, and one that does not yet
-- (the previous release, during an upgrade) still releases (06 §6.3).
CREATE FUNCTION procurement.value_released_items() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status = 'released' AND OLD.status <> 'released' THEN
    UPDATE procurement.purchase_order_item SET valuation_price = unit_price, valuation_price_unit = price_unit
    WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.id AND NOT is_removed AND valuation_price IS NULL;
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.value_released_items() OWNER TO erp_owner;
CREATE TRIGGER purchase_order_items_valued BEFORE UPDATE OF status ON procurement.purchase_order
  FOR EACH ROW EXECUTE FUNCTION procurement.value_released_items();

-- An item added to a released order (an order change, T47f) is valued as
-- it is added.
CREATE FUNCTION procurement.check_item_valued() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE number text;
BEGIN
  IF NEW.valuation_price IS NULL THEN
    SELECT document_number INTO number FROM procurement.purchase_order
    WHERE tenant_id = NEW.tenant_id AND id = NEW.purchase_order_id AND status <> 'draft';
    IF FOUND THEN
      RAISE EXCEPTION 'item % is added to released purchase order % with its valuation price', NEW.item_no, number
        USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_valued';
    END IF;
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_item_valued() OWNER TO erp_owner;
CREATE TRIGGER purchase_order_item_valued BEFORE INSERT ON procurement.purchase_order_item
  FOR EACH ROW EXECUTE FUNCTION procurement.check_item_valued();
