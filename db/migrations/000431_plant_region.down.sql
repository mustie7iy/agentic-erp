-- A plant loses its region: the previous version neither reads nor
-- writes it, and a sales item keeps the emirate it was entered or
-- defaulted with.
ALTER TABLE tenancy.org_unit
  DROP CONSTRAINT org_unit_region,
  DROP COLUMN region;
