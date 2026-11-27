-- Measures, removed ones too, which the previous version cannot hold, are
-- refused rather than dropped (06 §6.4: a down migration loses no data);
-- otherwise the down drops what 000495 added.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM materials.unit_measure) THEN
    RAISE EXCEPTION 'units of measure of materials carry dimensions, weights or GTINs; the previous version cannot hold them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TRIGGER unit_conversion_measures_removed ON materials.unit_conversion;
DROP FUNCTION materials.unit_conversion_measures_removed();
DROP TABLE materials.unit_measure;
DROP FUNCTION materials.unit_measure_of_unit();
DROP FUNCTION materials.gtin_valid(text);
