-- Validates the check 000469 added NOT VALID (06 §6.4), scanning the
-- table without blocking writes.
ALTER TABLE materials.material VALIDATE CONSTRAINT material_description_language_check;
