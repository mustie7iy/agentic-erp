-- A material's cycle counting indicator taken off, and the ABC analysis
-- applied (S2c3; ADR-0082 As built S2c3; 03-data-model §3.35): a
-- material's indicator at a plant is removed, at its version, and kept as
-- removed (04 §4.3: no hard deletes); the material is then no longer due
-- by an indicator there, and it may be given one again, a new row. A
-- material has one active indicator at a plant, which a partial unique
-- index of the constraint's name holds; a removed one never changes
-- again. The ABC analysis is computed on read; an analysis applied is
-- kept, insert-only, with the changes it made to the materials'
-- indicators. Expand only: a column with a default the previous version
-- neither reads nor writes, a key that refuses nothing the previous
-- version writes, and new tables (06 §6.4). During a rollout the previous
-- version reads a removed row as a material's indicator still, until it
-- is replaced.
ALTER TABLE inventory.cycle_count_material
  ADD COLUMN status text NOT NULL DEFAULT 'active' CONSTRAINT cycle_count_material_status_check CHECK (status IN ('active', 'removed'));
GRANT UPDATE (status) ON inventory.cycle_count_material TO erp_app;
ALTER TABLE inventory.cycle_count_material DROP CONSTRAINT cycle_count_material_once;
CREATE UNIQUE INDEX cycle_count_material_once ON inventory.cycle_count_material (tenant_id, plant_id, material_id) WHERE status = 'active';

-- A removed indicator stays as it was removed: its status, indicator and
-- version never change again.
CREATE FUNCTION inventory.cycle_count_material_removed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'removed' THEN
    RAISE EXCEPTION 'cycle_count_material_removed: material cycle count indicator % is removed; it does not change', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'cycle_count_material_removed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION inventory.cycle_count_material_removed() OWNER TO erp_owner;
CREATE TRIGGER cycle_count_material_removed BEFORE UPDATE ON inventory.cycle_count_material
  FOR EACH ROW EXECUTE FUNCTION inventory.cycle_count_material_removed();

UPDATE ref.permission SET description = 'Maintain cycle counting: the count indicators of plants and the indicators of materials at plants, '
  || 'given, changed, removed or set from an ABC analysis' WHERE code = 'inventory.count_plan.write';

-- An ABC analysis applied at a plant: its window, the company's shares
-- then, the plant's consumption value in the company's currency, the
-- preview's digest, who applied it and when. Insert-only.
CREATE TABLE inventory.cycle_count_abc_application (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  legal_entity_id    uuid NOT NULL,    -- tenancy context: the plant's company
  plant_id           uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  from_date          date NOT NULL,
  to_date            date NOT NULL,
  share_a            smallint NOT NULL,
  share_b            smallint NOT NULL,
  currency           char(3) NOT NULL REFERENCES ref.currency,
  total_consumption  bigint NOT NULL CONSTRAINT cycle_count_abc_application_total_check CHECK (total_consumption >= 0),
  preview_token      text NOT NULL CONSTRAINT cycle_count_abc_application_token_check CHECK (preview_token ~ '^[0-9a-f]{64}$'),
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- At most 366 days, ending where it starts or later.
  CONSTRAINT cycle_count_abc_application_window_check CHECK (to_date >= from_date AND to_date - from_date < 366),
  -- Each at least 1, together at most 99 (config.ABCShares).
  CONSTRAINT cycle_count_abc_application_shares_check CHECK (share_a >= 1 AND share_b >= 1 AND share_a + share_b <= 99)
);
-- The list: the tenant's applications, the latest first, in the order its pages are read.
CREATE INDEX cycle_count_abc_application_by_created_at ON inventory.cycle_count_abc_application (tenant_id, created_at, id);
CREATE TRIGGER cycle_count_abc_application_insert_only BEFORE UPDATE OR DELETE ON inventory.cycle_count_abc_application
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('inventory.cycle_count_abc_application'::regclass);

-- A change an application made, numbered in the analysis's order: the
-- material's consumption value and class, the indicator given, the one it
-- replaced (none for a material given its first) and the material's
-- indicator row it made or changed. Insert-only.
CREATE TABLE inventory.cycle_count_abc_change (
  tenant_id                uuid NOT NULL,
  application_id           uuid NOT NULL,
  line_no                  smallint NOT NULL CONSTRAINT cycle_count_abc_change_line_no_check CHECK (line_no BETWEEN 1 AND 1000),
  material_id              uuid NOT NULL,    -- materials context
  consumption              bigint NOT NULL CONSTRAINT cycle_count_abc_change_consumption_check CHECK (consumption >= 0),
  class                    text NOT NULL CONSTRAINT cycle_count_abc_change_class_check CHECK (class IN ('A', 'B', 'C')),
  indicator_id             uuid NOT NULL,
  before_indicator_id      uuid,
  cycle_count_material_id  uuid NOT NULL,
  PRIMARY KEY (tenant_id, application_id, line_no),
  CONSTRAINT cycle_count_abc_change_material_once UNIQUE (tenant_id, application_id, material_id),
  CONSTRAINT cycle_count_abc_change_application_fkey FOREIGN KEY (tenant_id, application_id)
    REFERENCES inventory.cycle_count_abc_application (tenant_id, id),
  CONSTRAINT cycle_count_abc_change_indicator_fkey FOREIGN KEY (tenant_id, indicator_id) REFERENCES inventory.cycle_count_indicator (tenant_id, id),
  CONSTRAINT cycle_count_abc_change_before_fkey FOREIGN KEY (tenant_id, before_indicator_id) REFERENCES inventory.cycle_count_indicator (tenant_id, id),
  CONSTRAINT cycle_count_abc_change_material_fkey FOREIGN KEY (tenant_id, cycle_count_material_id)
    REFERENCES inventory.cycle_count_material (tenant_id, id),
  -- A change replaces another indicator, or gives the first.
  CONSTRAINT cycle_count_abc_change_before_check CHECK (before_indicator_id IS DISTINCT FROM indicator_id)
);
-- The foreign keys' lookups.
CREATE INDEX cycle_count_abc_change_by_indicator ON inventory.cycle_count_abc_change (tenant_id, indicator_id);
CREATE INDEX cycle_count_abc_change_by_before ON inventory.cycle_count_abc_change (tenant_id, before_indicator_id);
CREATE INDEX cycle_count_abc_change_by_material ON inventory.cycle_count_abc_change (tenant_id, cycle_count_material_id);
CREATE TRIGGER cycle_count_abc_change_insert_only BEFORE UPDATE OR DELETE ON inventory.cycle_count_abc_change
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('inventory.cycle_count_abc_change'::regclass);
