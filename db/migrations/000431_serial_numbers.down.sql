-- Serial numbers and serial-managed materials the previous version cannot
-- hold are refused rather than dropped (06 §6.4: a down migration loses
-- no data). The guard reads every tenant's rows.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM inventory.serial) OR EXISTS (SELECT FROM materials.material WHERE serial_managed) THEN
    RAISE EXCEPTION 'serial numbers or serial-managed materials exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TRIGGER movement_serials ON inventory.movement;
DROP TABLE inventory.movement_serial;
DROP FUNCTION inventory.movement_serial_counted();
DROP FUNCTION inventory.movement_serials();
DROP FUNCTION inventory.check_movement_serials(uuid, uuid, smallint);
DROP FUNCTION inventory.movement_serial_moves();
DROP TABLE inventory.serial;
DROP FUNCTION inventory.serial_out_of_stock();
ALTER TABLE inventory.transit_stock DROP COLUMN serial_managed;
ALTER TABLE inventory.stock DROP COLUMN serial_managed;
-- material_fixed as 000428 left it.
CREATE OR REPLACE FUNCTION materials.material_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.number <> OLD.number OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.base_uom_code <> OLD.base_uom_code
     OR NEW.material_type <> OLD.material_type OR NEW.batch_managed <> OLD.batch_managed THEN
    RAISE EXCEPTION 'a material''s number, company, base unit, type and batch management do not change'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'material_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE materials.material DROP COLUMN serial_managed;
