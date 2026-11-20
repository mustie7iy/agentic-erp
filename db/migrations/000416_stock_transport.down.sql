DELETE FROM identity.sod_rule WHERE permission_a LIKE 'procurement.stock_transport.%' OR permission_b LIKE 'procurement.stock_transport.%';
DELETE FROM identity.role_permission WHERE permission_code LIKE 'procurement.stock_transport.%';
DELETE FROM ref.permission WHERE code LIKE 'procurement.stock_transport.%';
-- Stock transport orders and their movements the previous version cannot
-- hold are refused rather than dropped (06 §6.4: a down migration loses
-- no data).
DO $$
BEGIN
  IF EXISTS (SELECT FROM procurement.stock_transport_order) OR EXISTS (SELECT FROM inventory.movement WHERE in_transit)
     OR EXISTS (SELECT FROM inventory.material_document WHERE movement_type = '351') THEN
    RAISE EXCEPTION 'stock transport orders exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE procurement.stock_transport_order_item;
DROP TABLE procurement.stock_transport_order;
DROP FUNCTION procurement.stock_transport_order_fixed();
-- The checks go back to 000411's state, validated.
ALTER TABLE inventory.material_document
  DROP CONSTRAINT material_document_movement_type_check,
  ADD CONSTRAINT material_document_movement_type_check
    CHECK (movement_type IN ('101', '102', '501', '502', '561', '562', '201', '202', '551', '552', '311', '312', '301', '302', '601', '602', '701', '702')),
  DROP CONSTRAINT material_document_reference,
  ADD CONSTRAINT material_document_reference CHECK ((reference IS NOT NULL) = (movement_type IN ('101', '102', '601', '602')));
DROP TRIGGER count_posting_block ON inventory.movement;
CREATE TRIGGER count_posting_block BEFORE INSERT ON inventory.movement FOR EACH ROW EXECUTE FUNCTION inventory.count_posting_block();
-- movement_stock as 000377 left it.
CREATE OR REPLACE FUNCTION inventory.movement_stock() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE inventory.stock SET quantity = quantity + NEW.quantity, updated_at = now()
  WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement adds to a stock row written before it' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_stock';
  END IF;
  UPDATE inventory.valuation SET quantity = quantity + NEW.quantity, value = value + NEW.amount, version = version + 1,
    price = CASE WHEN price_control = 'moving_average' AND quantity + NEW.quantity > 0
      THEN div(2 * (value::numeric + NEW.amount) * price_unit + (quantity + NEW.quantity), 2 * (quantity + NEW.quantity))::bigint ELSE price END
  WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement is valued by its material''s valuation in the plant' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_valuation';
  END IF;
  RETURN NULL;
END $$;
DROP TABLE inventory.transit_stock;
ALTER TABLE inventory.movement DROP COLUMN in_transit;
