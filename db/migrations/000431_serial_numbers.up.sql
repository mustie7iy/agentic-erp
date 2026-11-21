-- Serial numbers (S2g; ADR-0082 As built S2g; 03-data-model §3.35). A
-- material has a serial number profile from its creation or never
-- (materials.material.serial_managed, SAP MARC's SERNP, from memory, kept
-- on the material and fixed as its batch management is): every unit of it
-- moved at a storage location is named by its serial number, unique for
-- the material (SAP EQUI's SERNR per material, from memory), and a
-- serial number is in stock at most once: inventory.serial holds where
-- it is now, at one storage location of a plant or in no stock, and only
-- its movements move it (inventory.movement_serial, SAP SER03's
-- history): a receipt of one in stock, or an issue of one not in stock
-- at the line's location, is refused. A movement of such a material names
-- as many serial numbers as its quantity is units, and a movement of
-- another material names none (movement_serials, checked at commit, as
-- the serial numbers are written after their movements). A stock row says
-- whether its material is serial-managed, written so by the application
-- as it writes the row and never changed, so the database needs no read
-- of the materials context's tables (02 §2.1); stock in transit holds no
-- serial numbers yet.
-- Expand only (06 §6.4): new tables, columns with defaults the previous
-- version never sets (it creates no serial-managed material and names no
-- serial number; a stock row it writes is not serial-managed, so a
-- serial-managed material is created once no previous version serves),
-- a check at commit that every movement the previous version writes
-- passes (none is of a serial-managed stock row or names a serial
-- number), and no new permission: serial numbers are moved by the
-- goods movements that name them and read with the stock.

ALTER TABLE materials.material ADD COLUMN serial_managed boolean NOT NULL DEFAULT false;
-- The number, its company, the base unit, the type, batch management and
-- the serial number profile stay as created.
CREATE OR REPLACE FUNCTION materials.material_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.number <> OLD.number OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.base_uom_code <> OLD.base_uom_code
     OR NEW.material_type <> OLD.material_type OR NEW.batch_managed <> OLD.batch_managed OR NEW.serial_managed <> OLD.serial_managed THEN
    RAISE EXCEPTION 'a material''s number, company, base unit, type, batch management and serial number profile do not change'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'material_fixed';
  END IF;
  RETURN NEW;
END $$;

-- Whether a stock row's material is serial-managed, as the application
-- writes the row; the application may not update it.
ALTER TABLE inventory.stock ADD COLUMN serial_managed boolean NOT NULL DEFAULT false;
ALTER TABLE inventory.transit_stock ADD COLUMN serial_managed boolean NOT NULL DEFAULT false;

-- A serial number of a serial-managed material: 1 to 18 capitals, digits,
-- '-' or '_' (SAP SERNR's length, from memory), once per material; where
-- it is in stock now (a storage location of a plant, both or neither),
-- and the company of the goods movement that last moved it, which a read
-- is filtered by. The application writes it out of stock, as a receipt
-- first names it; only its movements change where it is.
CREATE TABLE inventory.serial (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,    -- tenancy context: the company of its last movement
  material_id          uuid NOT NULL,    -- materials context: a serial-managed material
  number               text NOT NULL CONSTRAINT serial_number_check CHECK (number ~ '^[A-Z0-9_-]{1,18}$'),
  plant_id             uuid,             -- tenancy context: where it is in stock; null when it is in none
  storage_location_id  uuid,             -- tenancy context
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT serial_number_once UNIQUE (tenant_id, material_id, number),
  -- What a movement's serial number names: a serial number of its material.
  CONSTRAINT serial_of_material UNIQUE (tenant_id, id, material_id),
  CONSTRAINT serial_location CHECK ((plant_id IS NULL) = (storage_location_id IS NULL))
);
-- The list: the tenant's serial numbers in the order they were first received.
CREATE INDEX serial_by_created_at ON inventory.serial (tenant_id, created_at, id);
-- The serial numbers in stock at a location, for the list's filters.
CREATE INDEX serial_by_location ON inventory.serial (tenant_id, material_id, plant_id, storage_location_id);
SELECT platform.secure_table('inventory.serial'::regclass);
GRANT UPDATE (updated_at) ON inventory.serial TO erp_app;
CREATE FUNCTION inventory.serial_out_of_stock() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.plant_id IS NOT NULL THEN
    RAISE EXCEPTION 'a serial number is written out of stock; its receipt puts it in stock' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'serial_from_movements';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION inventory.serial_out_of_stock() OWNER TO erp_owner;
CREATE TRIGGER serial_from_movements BEFORE INSERT ON inventory.serial FOR EACH ROW EXECUTE FUNCTION inventory.serial_out_of_stock();
CREATE TRIGGER serial_kept BEFORE DELETE ON inventory.serial FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- A serial number a movement moves: received at its location (a positive
-- quantity) or issued from it (a negative one). Insert-only: the history
-- of a serial number is its movements.
CREATE TABLE inventory.movement_serial (
  tenant_id             uuid NOT NULL,
  material_document_id  uuid NOT NULL,
  line_no               smallint NOT NULL,
  serial_id             uuid NOT NULL,
  material_id           uuid NOT NULL,    -- materials context: the movement's
  created_at            timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, material_document_id, line_no, serial_id),
  CONSTRAINT movement_serial_movement_fkey FOREIGN KEY (tenant_id, material_document_id, line_no)
    REFERENCES inventory.movement (tenant_id, material_document_id, line_no),
  CONSTRAINT movement_serial_serial_fkey FOREIGN KEY (tenant_id, serial_id, material_id) REFERENCES inventory.serial (tenant_id, id, material_id)
);
CREATE TRIGGER movement_serial_insert_only BEFORE UPDATE OR DELETE ON inventory.movement_serial
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- A serial number's history, in the order it was moved.
CREATE INDEX movement_serial_by_serial ON inventory.movement_serial (tenant_id, serial_id, created_at);
SELECT platform.secure_table('inventory.movement_serial'::regclass);

-- A serial number's movement moves it: a receipt puts it in stock at the
-- movement's location, refused when it is in stock already, so it is in
-- stock at most once; an issue takes it out of stock, refused unless it
-- is in stock at that location. The movement is of its material and at a
-- storage location, not in transit.
CREATE FUNCTION inventory.movement_serial_moves() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  mv record;
BEGIN
  SELECT m.material_id, m.plant_id, m.storage_location_id, m.quantity, m.in_transit, d.legal_entity_id INTO mv
  FROM inventory.movement m JOIN inventory.material_document d ON d.tenant_id = m.tenant_id AND d.id = m.material_document_id
  WHERE m.tenant_id = NEW.tenant_id AND m.material_document_id = NEW.material_document_id AND m.line_no = NEW.line_no;
  IF mv.material_id <> NEW.material_id OR mv.in_transit THEN
    RAISE EXCEPTION 'a serial number is moved by a movement of its material at a storage location' USING ERRCODE = 'check_violation',
      CONSTRAINT = 'movement_serial_of_movement';
  END IF;
  IF mv.quantity > 0 THEN
    UPDATE inventory.serial SET plant_id = mv.plant_id, storage_location_id = mv.storage_location_id, legal_entity_id = mv.legal_entity_id,
      updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND id = NEW.serial_id AND plant_id IS NULL;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'a serial number is received when it is in no stock: it is in stock at most once' USING ERRCODE = 'check_violation',
        CONSTRAINT = 'serial_in_stock_once';
    END IF;
  ELSE
    UPDATE inventory.serial SET plant_id = NULL, storage_location_id = NULL, legal_entity_id = mv.legal_entity_id, updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND id = NEW.serial_id AND plant_id = mv.plant_id AND storage_location_id = mv.storage_location_id;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'a serial number is issued from the location it is in stock at' USING ERRCODE = 'check_violation',
        CONSTRAINT = 'serial_issued_from_stock';
    END IF;
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION inventory.movement_serial_moves() OWNER TO erp_owner;
CREATE TRIGGER movement_serial_moves AFTER INSERT ON inventory.movement_serial FOR EACH ROW EXECUTE FUNCTION inventory.movement_serial_moves();

-- At commit, a movement of a serial-managed stock row names as many
-- serial numbers as its quantity is units, and any other movement none
-- (a movement in transit is of a material not serial-managed). Checked
-- for each movement written and for each movement a serial number is
-- added to, so a serial number added later to a committed line is
-- refused too.
CREATE FUNCTION inventory.check_movement_serials(tenant uuid, document uuid, line smallint) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  mv record;
  managed boolean;
  named bigint;
BEGIN
  SELECT material_id, plant_id, storage_location_id, quantity, in_transit INTO mv FROM inventory.movement
  WHERE tenant_id = tenant AND material_document_id = document AND line_no = line;
  IF mv.in_transit THEN
    SELECT serial_managed INTO managed FROM inventory.transit_stock
    WHERE tenant_id = tenant AND material_id = mv.material_id AND plant_id = mv.plant_id AND storage_location_id = mv.storage_location_id;
  ELSE
    SELECT serial_managed INTO managed FROM inventory.stock
    WHERE tenant_id = tenant AND material_id = mv.material_id AND plant_id = mv.plant_id AND storage_location_id = mv.storage_location_id;
  END IF;
  SELECT count(*) INTO named FROM inventory.movement_serial
  WHERE tenant_id = tenant AND material_document_id = document AND line_no = line;
  IF (managed AND (mv.in_transit OR named <> abs(mv.quantity))) OR (NOT managed AND named <> 0) THEN
    RAISE EXCEPTION 'a movement of a serial-managed material names a serial number for each unit it moves, and no other movement names one'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'movement_serials';
  END IF;
END $$;
ALTER FUNCTION inventory.check_movement_serials(uuid, uuid, smallint) OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION inventory.check_movement_serials(uuid, uuid, smallint) FROM PUBLIC;
CREATE FUNCTION inventory.movement_serials() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  PERFORM inventory.check_movement_serials(NEW.tenant_id, NEW.material_document_id, NEW.line_no);
  RETURN NULL;
END $$;
ALTER FUNCTION inventory.movement_serials() OWNER TO erp_owner;
CREATE CONSTRAINT TRIGGER movement_serials AFTER INSERT ON inventory.movement
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION inventory.movement_serials();
CREATE CONSTRAINT TRIGGER movement_serial_counted AFTER INSERT ON inventory.movement_serial
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION inventory.movement_serials();
