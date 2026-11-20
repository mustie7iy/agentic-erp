-- Batches (S2f; ADR-0082 As built S2f; 03-data-model §3.35, §3.36). A
-- material is managed in batches from its creation or never
-- (materials.material.batch_managed, SAP MARA's XCHPF, from memory; fixed
-- as its base unit is). A batch (SAP MCH1, batch level "material", from
-- memory) is a lot of one material with a number unique for that
-- material, its manufacture date and its expiry date. Every movement of
-- a batch-managed material at a storage location names a batch of it, and
-- a movement of another material names none; the movement adds to the
-- batch's stock there (inventory.batch_stock, SAP MCHB) as it adds to the
-- location's (inventory.stock, MARD), so the batches at a location add up
-- to its stock. A stock row says whether its material is batch-managed,
-- written so by the application as it writes the row and never changed,
-- so the database refuses a movement that would leave the batches short
-- of the location without reading the materials context's tables (no
-- reference leaves a schema but ref's, 02 §2.1). Stock in transit (S2h)
-- holds no batches yet: a batch-managed material is not moved in transit.
-- Expand only (06 §6.4): new tables, columns with defaults the previous
-- version never sets (every material and stock row it writes is not
-- batch-managed, and none of its movements names a batch), a foreign key
-- and a check added NOT VALID (validated by 000422), a movement_stock that
-- does for every movement the previous version writes what it did, and a
-- permission it neither reads nor writes.

ALTER TABLE materials.material ADD COLUMN batch_managed boolean NOT NULL DEFAULT false;
-- The number, its company, the base unit, the type and batch management
-- stay as created.
CREATE OR REPLACE FUNCTION materials.material_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.number <> OLD.number OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.base_uom_code <> OLD.base_uom_code
     OR NEW.material_type <> OLD.material_type OR NEW.batch_managed <> OLD.batch_managed THEN
    RAISE EXCEPTION 'a material''s number, company, base unit, type and batch management do not change'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'material_fixed';
  END IF;
  RETURN NEW;
END $$;

-- A batch of a material: its number, given by the person who creates it
-- (a supplier's or the plant's), 1 to 10 capitals, digits, '-' or '_'
-- (SAP CHARG's length, from memory), its manufacture date and the expiry
-- date it is used by, when it has one, never before it was made.
-- Insert-only: a changed date is Later.
CREATE TABLE inventory.batch (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  material_id       uuid NOT NULL,    -- materials context: a batch-managed material
  number            text NOT NULL CONSTRAINT batch_number_check CHECK (number ~ '^[A-Z0-9_-]{1,10}$'),
  manufacture_date  date NOT NULL,
  expiry_date       date,
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT batch_number_once UNIQUE (tenant_id, material_id, number),
  -- What a movement and a batch stock row name: a batch of their material.
  CONSTRAINT batch_of_material UNIQUE (tenant_id, id, material_id),
  CONSTRAINT batch_dates CHECK (expiry_date >= manufacture_date)
);
CREATE TRIGGER batch_insert_only BEFORE UPDATE OR DELETE ON inventory.batch
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- The list: the tenant's batches, the latest first, in the order its pages are read.
CREATE INDEX batch_by_created_at ON inventory.batch (tenant_id, created_at, id);
SELECT platform.secure_table('inventory.batch'::regclass);

-- A movement of a batch-managed material names its batch, one of its
-- material; a line in transit names none.
ALTER TABLE inventory.movement ADD COLUMN batch_id uuid,
  ADD CONSTRAINT movement_batch_fkey FOREIGN KEY (tenant_id, batch_id, material_id) REFERENCES inventory.batch (tenant_id, id, material_id) NOT VALID,
  ADD CONSTRAINT movement_batch_not_in_transit CHECK (batch_id IS NULL OR NOT in_transit) NOT VALID;
-- A batch's movements, for its stock as at a date (Later) and where it went.
CREATE INDEX movement_by_batch ON inventory.movement (tenant_id, batch_id) WHERE batch_id IS NOT NULL;

-- Whether a stock row's material is managed in batches, as the
-- application writes the row; the application may not update it.
ALTER TABLE inventory.stock ADD COLUMN batch_managed boolean NOT NULL DEFAULT false;
ALTER TABLE inventory.transit_stock ADD COLUMN batch_managed boolean NOT NULL DEFAULT false;

-- The stock of a batch at a storage location of a plant (SAP MCHB's
-- unrestricted quantity), in its material's base unit, with the plant's
-- company. As inventory.stock: the application writes a row with no
-- quantity before its first movement and locks it; only its movements
-- change its quantity, never below zero.
CREATE TABLE inventory.batch_stock (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,    -- tenancy context: the plant's company
  material_id          uuid NOT NULL,    -- materials context
  plant_id             uuid NOT NULL,    -- tenancy context
  storage_location_id  uuid NOT NULL,    -- tenancy context
  batch_id             uuid NOT NULL,
  quantity             numeric(20,6) NOT NULL DEFAULT 0 CONSTRAINT batch_stock_not_negative CHECK (quantity >= 0),
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT batch_stock_once UNIQUE (tenant_id, material_id, plant_id, storage_location_id, batch_id),
  CONSTRAINT batch_stock_batch_fkey FOREIGN KEY (tenant_id, batch_id, material_id) REFERENCES inventory.batch (tenant_id, id, material_id)
);
-- The list: the tenant's batch stock in the order it was first moved.
CREATE INDEX batch_stock_by_created_at ON inventory.batch_stock (tenant_id, created_at, id);
-- A batch's stock, for the list's batch filter.
CREATE INDEX batch_stock_by_batch ON inventory.batch_stock (tenant_id, batch_id);
SELECT platform.secure_table('inventory.batch_stock'::regclass);
GRANT UPDATE (updated_at) ON inventory.batch_stock TO erp_app;
CREATE TRIGGER batch_stock_from_movements BEFORE INSERT ON inventory.batch_stock FOR EACH ROW EXECUTE FUNCTION inventory.stock_empty();

-- A movement adds its quantity to its stock row, or to its transit stock
-- row when it is in transit, as 000418 has it; a movement at a location
-- names a batch exactly when the row's material is batch-managed, and
-- adds to that batch's stock there too; one in transit is of a material
-- not batch-managed. Then its quantity and value go to its material's
-- valuation in the plant as 000418 has it.
CREATE OR REPLACE FUNCTION inventory.movement_stock() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  managed boolean;
BEGIN
  IF NEW.in_transit THEN
    UPDATE inventory.transit_stock SET quantity = quantity + NEW.quantity, updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id
    RETURNING batch_managed INTO managed;
  ELSE
    UPDATE inventory.stock SET quantity = quantity + NEW.quantity, updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id
    RETURNING batch_managed INTO managed;
  END IF;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement adds to a stock row written before it' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_stock';
  END IF;
  IF managed <> (NEW.batch_id IS NOT NULL) OR (managed AND NEW.in_transit) THEN
    RAISE EXCEPTION 'a movement of a batch-managed material at a storage location names its batch, and no other movement names one'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'movement_batch';
  END IF;
  IF NEW.batch_id IS NOT NULL THEN
    UPDATE inventory.batch_stock SET quantity = quantity + NEW.quantity, updated_at = now()
    WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id
      AND batch_id = NEW.batch_id;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'a movement of a batch adds to a batch stock row written before it' USING ERRCODE = 'foreign_key_violation',
        CONSTRAINT = 'movement_batch_stock';
    END IF;
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

-- Creating batches of batch-managed materials; they are read with the
-- stock (inventory.stock.read).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('inventory.batch.write', 'Create batches of batch-managed materials, with their manufacture and expiry dates', false);
