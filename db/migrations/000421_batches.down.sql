DELETE FROM identity.sod_rule WHERE permission_a = 'inventory.batch.write' OR permission_b = 'inventory.batch.write';
DELETE FROM identity.role_permission WHERE permission_code = 'inventory.batch.write';
DELETE FROM ref.permission WHERE code = 'inventory.batch.write';
-- Batches and batch-managed materials the previous version cannot hold
-- are refused rather than dropped (06 §6.4: a down migration loses no
-- data).
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.batch) OR EXISTS (SELECT FROM materials.material WHERE batch_managed) THEN
    RAISE EXCEPTION 'batches or batch-managed materials exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
-- movement_stock as 000418 left it.
CREATE OR REPLACE FUNCTION inventory.movement_stock() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF NEW.in_transit THEN
    UPDATE inventory.transit_stock SET quantity = quantity + NEW.quantity, updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id;
  ELSE
    UPDATE inventory.stock SET quantity = quantity + NEW.quantity, updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id;
  END IF;
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
DROP TABLE inventory.batch_stock;
ALTER TABLE inventory.transit_stock DROP COLUMN batch_managed;
ALTER TABLE inventory.stock DROP COLUMN batch_managed;
ALTER TABLE inventory.movement DROP COLUMN batch_id;
DROP TABLE inventory.batch;
CREATE OR REPLACE FUNCTION materials.material_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.number <> OLD.number OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.base_uom_code <> OLD.base_uom_code
     OR NEW.material_type <> OLD.material_type THEN
    RAISE EXCEPTION 'a material''s number, company, base unit and type do not change' USING ERRCODE = 'check_violation', CONSTRAINT = 'material_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE materials.material DROP COLUMN batch_managed;
