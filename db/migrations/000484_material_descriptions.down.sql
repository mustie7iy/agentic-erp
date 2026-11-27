-- Further descriptions and defaults in another language than English,
-- which the previous version cannot hold, are refused (06 §6.4: a down
-- migration loses no data); otherwise the down drops what 000484 added.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM materials.material_description) OR EXISTS (SELECT FROM materials.material WHERE description_language <> 'en') THEN
    RAISE EXCEPTION 'materials described in further languages, or by default in another than en, exist; the previous version cannot hold them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE materials.material_description;
DROP FUNCTION materials.material_description_not_default();
DROP TRIGGER material_language_fixed ON materials.material;
DROP FUNCTION materials.material_language_fixed();
ALTER TABLE materials.material DROP COLUMN description_language;
