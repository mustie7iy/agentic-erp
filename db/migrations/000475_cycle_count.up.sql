-- Cycle counting indicators (S2c2; ADR-0082 As built S2c2; 03-data-model
-- §3.35): a plant's indicators (SAP T159C, from memory), each a code with
-- the interval in days between counts of a material that carries it, and
-- a material's indicator at a plant (SAP MARC-ABCIN, from memory), one
-- per material and plant, of an indicator of that plant. A count of the
-- material at a storage location of the plant falls due once the
-- interval has passed since its last posted count there (the read
-- GET /v1/cycle-counts-due computes it; nothing is stored). Plants and
-- legal_entity_id are the tenancy context's, materials the materials
-- context's, by id (02 §2.1). Nothing is deleted; what an indicator or a
-- material's indicator is of does not change, which the column grants
-- hold. Expand only: new tables and a permission the previous version
-- neither reads nor writes (06 §6.4).
CREATE TABLE inventory.cycle_count_indicator (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,    -- tenancy context: the plant's company
  plant_id         uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  code             text NOT NULL CONSTRAINT cycle_count_indicator_code_check CHECK (code ~ '^[A-Z0-9]{1,4}$'),
  -- At most a year: a material with an indicator is counted at least once a year.
  interval_days    integer NOT NULL CONSTRAINT cycle_count_indicator_interval_check CHECK (interval_days BETWEEN 1 AND 366),
  version          integer NOT NULL DEFAULT 1 CONSTRAINT cycle_count_indicator_version_check CHECK (version >= 1),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT cycle_count_indicator_code_once UNIQUE (tenant_id, plant_id, code),
  -- The key a material's indicator names it by with its plant (cycle_count_material_indicator_fkey).
  CONSTRAINT cycle_count_indicator_plant UNIQUE (tenant_id, id, plant_id)
);
-- The list: the tenant's indicators in the order they were created, in the order its pages are read.
CREATE INDEX cycle_count_indicator_by_created_at ON inventory.cycle_count_indicator (tenant_id, created_at, id);
CREATE TRIGGER cycle_count_indicator_touch BEFORE UPDATE ON inventory.cycle_count_indicator FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('inventory.cycle_count_indicator'::regclass);
REVOKE UPDATE ON inventory.cycle_count_indicator FROM erp_app;
GRANT UPDATE (interval_days, version, updated_by) ON inventory.cycle_count_indicator TO erp_app;

-- A material's indicator at a plant: an indicator of the same plant (the
-- foreign key names the plant with it), changed to another of the plant.
CREATE TABLE inventory.cycle_count_material (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,    -- tenancy context: the plant's company
  plant_id         uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  material_id      uuid NOT NULL,    -- materials context
  indicator_id     uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1 CONSTRAINT cycle_count_material_version_check CHECK (version >= 1),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT cycle_count_material_once UNIQUE (tenant_id, plant_id, material_id),
  CONSTRAINT cycle_count_material_indicator_fkey FOREIGN KEY (tenant_id, indicator_id, plant_id)
    REFERENCES inventory.cycle_count_indicator (tenant_id, id, plant_id)
);
-- The list: the tenant's materials' indicators in the order they were created, in the order its pages are read.
CREATE INDEX cycle_count_material_by_created_at ON inventory.cycle_count_material (tenant_id, created_at, id);
-- The list's indicator filter, and the foreign key's lookup.
CREATE INDEX cycle_count_material_by_indicator ON inventory.cycle_count_material (tenant_id, indicator_id);
CREATE TRIGGER cycle_count_material_touch BEFORE UPDATE ON inventory.cycle_count_material FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('inventory.cycle_count_material'::regclass);
REVOKE UPDATE ON inventory.cycle_count_material FROM erp_app;
GRANT UPDATE (indicator_id, version, updated_by) ON inventory.cycle_count_material TO erp_app;

-- Maintaining cycle counting (ADR-0082 As built S2c2); reading it and the
-- counts due is inventory.stock.read's.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('inventory.count_plan.write', 'Maintain cycle counting: the count indicators of plants and the indicators of materials at plants', false);
