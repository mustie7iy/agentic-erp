DELETE FROM identity.sod_rule WHERE permission_a = 'materials.change.approve' OR permission_b = 'materials.change.approve';
DELETE FROM identity.role_permission WHERE permission_code = 'materials.change.approve';
DELETE FROM ref.permission WHERE code = 'materials.change.approve';
DROP TABLE materials.material_change;
DROP INDEX materials.material_normalized_description;
DROP FUNCTION platform.normalized_text(text);
-- A pending material fails the narrowed check: its create change is decided
-- first.
ALTER TABLE materials.material DROP CONSTRAINT material_status_check,
  ADD CONSTRAINT material_status_check CHECK (status IN ('active', 'blocked'));
