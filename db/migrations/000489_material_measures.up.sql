-- Dimensions, weights and GTINs (S1a3; ADR-0082 As built S1a3;
-- 03-data-model §3.34): each unit of measure a material is kept in, its
-- base unit and each alternative unit, may carry its gross and net weight
-- in one weight unit, its volume in a volume unit, its length, width and
-- height in one length unit and its GTIN (SAP MARM's BRGEW, GEWEI, VOLUM,
-- VOLEH, LAENG, BREIT, HOEHE, MEABM and EAN11, and MARA's NTGEW, from
-- memory). Each value is a decimal above zero of up to ten whole digits
-- and three decimals (SAP's QUAN 13,3, from memory); a unit is given with
-- its values and only then; a net weight is no more than the gross one.
--
-- Expand only (06 §6.4): a new table the previous version neither reads
-- nor writes. Its one link to the previous version's writes is
-- unit_conversion_measures_removed, which takes the measures off an
-- alternative unit the previous version removes, so no GTIN stays held by
-- a unit no material has.
CREATE FUNCTION materials.gtin_valid(g text) RETURNS boolean
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT AS $$
  -- GS1's mod 10 (internal/platform/gtin): right-justified in 14 digits,
  -- the 13 before the check digit weighted 3, 1, 3, … from the left.
  SELECT CASE WHEN g ~ '^([0-9]{8}|[0-9]{12,14})$' THEN
    (10 - (SELECT sum(substr(lpad(g, 14, '0'), i, 1)::int * CASE WHEN i % 2 = 1 THEN 3 ELSE 1 END) FROM generate_series(1, 13) AS i) % 10) % 10
      = substr(g, length(g), 1)::int
  ELSE false END
$$;

CREATE TABLE materials.unit_measure (
  tenant_id       uuid NOT NULL,
  material_id     uuid NOT NULL,
  uom_code        text NOT NULL REFERENCES ref.unit_of_measure,
  gross_weight    numeric(13,3) CONSTRAINT unit_measure_gross_weight_check CHECK (gross_weight > 0),
  net_weight      numeric(13,3) CONSTRAINT unit_measure_net_weight_check CHECK (net_weight > 0),
  weight_uom      text REFERENCES ref.unit_of_measure,
  volume          numeric(13,3) CONSTRAINT unit_measure_volume_check CHECK (volume > 0),
  volume_uom      text REFERENCES ref.unit_of_measure,
  length          numeric(13,3) CONSTRAINT unit_measure_length_check CHECK (length > 0),
  width           numeric(13,3) CONSTRAINT unit_measure_width_check CHECK (width > 0),
  height          numeric(13,3) CONSTRAINT unit_measure_height_check CHECK (height > 0),
  dimension_uom   text REFERENCES ref.unit_of_measure,
  gtin            text CONSTRAINT unit_measure_gtin_check CHECK (materials.gtin_valid(gtin)),
  created_at      timestamptz NOT NULL DEFAULT now(),
  created_by      uuid NOT NULL,
  removed_at      timestamptz,
  removed_by      uuid,
  CONSTRAINT unit_measure_removed_check CHECK ((removed_at IS NULL) = (removed_by IS NULL)),
  CONSTRAINT unit_measure_weight_uom_check CHECK ((weight_uom IS NULL) = (gross_weight IS NULL AND net_weight IS NULL)),
  CONSTRAINT unit_measure_volume_uom_check CHECK ((volume_uom IS NULL) = (volume IS NULL)),
  CONSTRAINT unit_measure_dimension_uom_check CHECK ((dimension_uom IS NULL) = (length IS NULL AND width IS NULL AND height IS NULL)),
  CONSTRAINT unit_measure_net_weight_gross CHECK (net_weight <= gross_weight),
  CONSTRAINT unit_measure_something CHECK (num_nonnulls(gross_weight, net_weight, volume, length, width, height, gtin) > 0),
  PRIMARY KEY (tenant_id, material_id, uom_code),
  CONSTRAINT unit_measure_material_fkey FOREIGN KEY (tenant_id, material_id) REFERENCES materials.material (tenant_id, id)
);
-- A GTIN names one unit of one material in the tenant, compared as GS1
-- compares them, right-justified in 14 digits; a removed unit's no longer
-- counts. It also serves listMaterials' gtin filter.
CREATE UNIQUE INDEX unit_measure_gtin_once ON materials.unit_measure (tenant_id, lpad(gtin, 14, '0'))
  WHERE gtin IS NOT NULL AND removed_at IS NULL;

-- A unit's measures are of the material's base unit or of an alternative
-- unit it has, and each unit is of its kind: a weight unit of mass, a
-- volume unit of volume, a length unit of length. Every writer of a
-- material's units holds the material's row lock (materials' load), so
-- the units read here do not change under the check.
CREATE FUNCTION materials.unit_measure_of_unit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.removed_at IS NULL AND NOT EXISTS (SELECT FROM materials.material m
      WHERE m.tenant_id = NEW.tenant_id AND m.id = NEW.material_id AND m.base_uom_code = NEW.uom_code)
    AND NOT EXISTS (SELECT FROM materials.unit_conversion u
      WHERE u.tenant_id = NEW.tenant_id AND u.material_id = NEW.material_id AND u.uom_code = NEW.uom_code AND u.removed_at IS NULL) THEN
    RAISE EXCEPTION 'measures are of the material''s base unit or of an alternative unit it has'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'unit_measure_of_unit';
  END IF;
  IF EXISTS (SELECT FROM (VALUES (NEW.weight_uom, 'mass'), (NEW.volume_uom, 'volume'), (NEW.dimension_uom, 'length')) AS x (code, dimension)
      JOIN ref.unit_of_measure r ON r.code = x.code WHERE r.dimension <> x.dimension) THEN
    RAISE EXCEPTION 'a weight is in a unit of mass, a volume in one of volume, a length in one of length'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'unit_measure_dimension';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER unit_measure_of_unit BEFORE INSERT OR UPDATE ON materials.unit_measure
  FOR EACH ROW EXECUTE FUNCTION materials.unit_measure_of_unit();
SELECT platform.secure_table('materials.unit_measure'::regclass);
-- Changed measures are updated in place, removed ones marked (nothing is
-- deleted, 000011).
GRANT UPDATE (gross_weight, net_weight, weight_uom, volume, volume_uom, length, width, height, dimension_uom, gtin, removed_at, removed_by)
  ON materials.unit_measure TO erp_app;

-- An alternative unit taken off a material takes its measures with it,
-- by whoever took it off, whichever version did.
CREATE FUNCTION materials.unit_conversion_measures_removed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  UPDATE materials.unit_measure SET removed_at = NEW.removed_at, removed_by = NEW.removed_by
    WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND uom_code = NEW.uom_code AND removed_at IS NULL;
  RETURN NULL;
END $$;
CREATE TRIGGER unit_conversion_measures_removed AFTER UPDATE OF removed_at ON materials.unit_conversion
  FOR EACH ROW WHEN (OLD.removed_at IS NULL AND NEW.removed_at IS NOT NULL) EXECUTE FUNCTION materials.unit_conversion_measures_removed();
