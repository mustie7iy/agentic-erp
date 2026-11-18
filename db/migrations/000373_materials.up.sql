-- The material master (S1a; ADR-0082 §1, §4-§6; 03-data-model §3.34):
-- the materials context's schema with its outbox and inbox, a material
-- (SAP MARA with MAKT's one description), the plants it is extended to
-- (MARC), its storage locations there (MARD without stock) and its
-- alternative units of measure (MARM). Plants and storage locations are
-- tenancy's org units and legal_entity_id the tenancy context's, by id:
-- no foreign key leaves the schema but ref's (02 §2.1). Expand only: a
-- new schema and two permissions, which the previous version neither
-- reads nor writes (06 §6.4).
CREATE SCHEMA materials;
GRANT USAGE ON SCHEMA materials TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('materials');
SELECT platform.secure_table('materials.outbox'::regclass);
SELECT platform.secure_table('materials.inbox'::regclass);
GRANT UPDATE (published_at) ON materials.outbox TO erp_app;
GRANT USAGE ON SCHEMA materials TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON materials.outbox TO erp_relay;

-- A material: tenant-wide master data, numbered from the material range
-- of the company that created it (legal_entity_id) and unique in the
-- tenant (ADR-0082 §6). The base unit and the type do not change once
-- created (material_fixed).
CREATE TABLE materials.material (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  number           text NOT NULL CONSTRAINT material_number_check CHECK (length(number) BETWEEN 1 AND 40),
  legal_entity_id  uuid NOT NULL,    -- tenancy context: the company whose range numbered it
  description      text NOT NULL CONSTRAINT material_description_check CHECK (length(description) BETWEEN 1 AND 200 AND length(trim(description)) > 0),
  base_uom_code    text NOT NULL REFERENCES ref.unit_of_measure,
  material_type    text NOT NULL CONSTRAINT material_type_check CHECK (material_type IN ('raw_material', 'semi_finished', 'finished_good', 'trading_good', 'non_stock', 'service')),
  material_group   text NOT NULL CONSTRAINT material_group_check CHECK (material_group ~ '^[A-Z0-9_-]{1,9}$'),
  status           text NOT NULL CONSTRAINT material_status_check CHECK (status IN ('active', 'blocked')),
  version          integer NOT NULL DEFAULT 1 CONSTRAINT material_version_check CHECK (version >= 1),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT material_number_once UNIQUE (tenant_id, number)
);
CREATE TRIGGER material_touch BEFORE UPDATE ON materials.material FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The number, its company, the base unit and the type stay as created.
CREATE FUNCTION materials.material_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.number <> OLD.number OR NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.base_uom_code <> OLD.base_uom_code
     OR NEW.material_type <> OLD.material_type THEN
    RAISE EXCEPTION 'a material''s number, company, base unit and type do not change' USING ERRCODE = 'check_violation', CONSTRAINT = 'material_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER material_fixed BEFORE UPDATE ON materials.material FOR EACH ROW EXECUTE FUNCTION materials.material_fixed();
-- material_number_once serves the list by number; a material group's and
-- a type's materials are filtered from it.
SELECT platform.secure_table('materials.material'::regclass);

-- The material at a plant (MARC): legal_entity_id is the plant's company,
-- kept so a read shows only the plants of companies the caller may act in.
-- Nothing is deleted (000011): a plant taken off the material is marked
-- removed, and given back by clearing the mark.
CREATE TABLE materials.plant_data (
  tenant_id        uuid NOT NULL,
  material_id      uuid NOT NULL,
  plant_id         uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  legal_entity_id  uuid NOT NULL,    -- tenancy context: the plant's company
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  removed_at       timestamptz,
  removed_by       uuid,
  CONSTRAINT plant_data_removed_check CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
  PRIMARY KEY (tenant_id, material_id, plant_id),
  CONSTRAINT plant_data_material_fkey FOREIGN KEY (tenant_id, material_id) REFERENCES materials.material (tenant_id, id)
);
-- A plant's materials, for the list's plant filter.
CREATE INDEX plant_data_by_plant ON materials.plant_data (tenant_id, plant_id, material_id);
SELECT platform.secure_table('materials.plant_data'::regclass);
GRANT UPDATE (removed_at, removed_by) ON materials.plant_data TO erp_app;

-- The material at a storage location of one of its plants (MARD's key;
-- the stock is the inventory context's, S1c).
CREATE TABLE materials.storage_location_data (
  tenant_id            uuid NOT NULL,
  material_id          uuid NOT NULL,
  plant_id             uuid NOT NULL,
  storage_location_id  uuid NOT NULL,    -- tenancy context: an org unit of kind storage_location under the plant
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  removed_at           timestamptz,
  removed_by           uuid,
  CONSTRAINT storage_location_data_removed_check CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
  PRIMARY KEY (tenant_id, material_id, storage_location_id),
  CONSTRAINT storage_location_data_plant_fkey FOREIGN KEY (tenant_id, material_id, plant_id)
    REFERENCES materials.plant_data (tenant_id, material_id, plant_id)
);
CREATE INDEX storage_location_data_by_plant ON materials.storage_location_data (tenant_id, material_id, plant_id);
SELECT platform.secure_table('materials.storage_location_data'::regclass);
GRANT UPDATE (removed_at, removed_by) ON materials.storage_location_data TO erp_app;

-- An alternative unit of measure (MARM): one uom_code is numerator /
-- denominator base units, both whole numbers (ADR-0082 §5). The base unit
-- has no row (unit_conversion_not_base). A removed unit is marked, as a
-- removed plant is.
CREATE TABLE materials.unit_conversion (
  tenant_id    uuid NOT NULL,
  material_id  uuid NOT NULL,
  uom_code     text NOT NULL REFERENCES ref.unit_of_measure,
  numerator    integer NOT NULL CONSTRAINT unit_conversion_numerator_check CHECK (numerator BETWEEN 1 AND 1000000),
  denominator  integer NOT NULL CONSTRAINT unit_conversion_denominator_check CHECK (denominator BETWEEN 1 AND 1000000),
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  removed_at   timestamptz,
  removed_by   uuid,
  CONSTRAINT unit_conversion_removed_check CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
  PRIMARY KEY (tenant_id, material_id, uom_code),
  CONSTRAINT unit_conversion_material_fkey FOREIGN KEY (tenant_id, material_id) REFERENCES materials.material (tenant_id, id)
);
CREATE FUNCTION materials.unit_conversion_not_base() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT FROM materials.material m WHERE m.tenant_id = NEW.tenant_id AND m.id = NEW.material_id AND m.base_uom_code = NEW.uom_code) THEN
    RAISE EXCEPTION 'the base unit has no conversion' USING ERRCODE = 'check_violation', CONSTRAINT = 'unit_conversion_not_base';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER unit_conversion_not_base BEFORE INSERT OR UPDATE ON materials.unit_conversion FOR EACH ROW EXECUTE FUNCTION materials.unit_conversion_not_base();
SELECT platform.secure_table('materials.unit_conversion'::regclass);
-- A changed conversion is updated in place, a removed unit marked.
GRANT UPDATE (numerator, denominator, removed_at, removed_by) ON materials.unit_conversion TO erp_app;

-- Reading materials and creating and changing them (ADR-0082 §4).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('materials.material.read', 'View materials with their plants, storage locations and units of measure', false),
  ('materials.material.write', 'Create and change materials', false);
