-- Further descriptions and defaults in another language than English,
-- which the previous version cannot hold, are refused rather than dropped
-- (06 §6.4: a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM materials.material_description) OR EXISTS (SELECT FROM materials.material WHERE description_language <> 'en') THEN
    RAISE EXCEPTION 'materials described in further languages, or by default in another than en, exist; the previous version cannot hold them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
