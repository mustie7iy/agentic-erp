-- Returns to the supplier (T47b, 03-data-model §3.8). A 122 sends goods
-- received on an order back, with its reason; a 123 reverses a 122, as a
-- 102 reverses a 101. A 102 reverses only a 101 and a 123 only a 122, and
-- each movement moves its quantity its own way: 101 and 123 take goods in
-- (positive), 102 and 122 send them out (negative). check_receipt (000006)
-- keeps the received quantity between zero and the tolerance, so a return
-- never exceeds what was received.
ALTER TABLE procurement.goods_movement DROP CONSTRAINT goods_movement_movement_type_check,
  ADD CONSTRAINT goods_movement_movement_type_check CHECK (movement_type IN ('101', '102', '122', '123')),
  DROP CONSTRAINT goods_movement_check,
  ADD CONSTRAINT goods_movement_reversal_pair CHECK ((movement_type IN ('102', '123')) = (reversal_of_id IS NOT NULL)),
  ADD COLUMN reason text CHECK (length(reason) BETWEEN 1 AND 200),
  ADD CONSTRAINT goods_movement_return_reason CHECK ((movement_type = '122') = (reason IS NOT NULL));

CREATE FUNCTION procurement.check_movement_reversal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.reversal_of_id IS NOT NULL AND (NEW.movement_type, (SELECT movement_type FROM procurement.goods_movement
      WHERE tenant_id = NEW.tenant_id AND id = NEW.reversal_of_id)) NOT IN (('102', '101'), ('123', '122')) THEN
    RAISE EXCEPTION 'a % does not reverse that movement', NEW.movement_type
      USING ERRCODE = 'check_violation', CONSTRAINT = 'goods_movement_reversal_type';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_movement_reversal() OWNER TO erp_owner;
CREATE TRIGGER goods_movement_reversal_type BEFORE INSERT ON procurement.goods_movement
  FOR EACH ROW EXECUTE FUNCTION procurement.check_movement_reversal();

CREATE FUNCTION procurement.check_movement_item_sign() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE t text;
BEGIN
  SELECT movement_type INTO t FROM procurement.goods_movement WHERE tenant_id = NEW.tenant_id AND id = NEW.goods_movement_id;
  IF (t IN ('101', '123')) <> (NEW.quantity > 0) THEN
    RAISE EXCEPTION 'a % moves its quantity the other way', t
      USING ERRCODE = 'check_violation', CONSTRAINT = 'goods_movement_item_sign';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_movement_item_sign() OWNER TO erp_owner;
CREATE TRIGGER goods_movement_item_sign BEFORE INSERT ON procurement.goods_movement_item
  FOR EACH ROW EXECUTE FUNCTION procurement.check_movement_item_sign();
