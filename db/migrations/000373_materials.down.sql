DELETE FROM identity.sod_rule WHERE permission_a IN ('materials.material.read', 'materials.material.write')
  OR permission_b IN ('materials.material.read', 'materials.material.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('materials.material.read', 'materials.material.write');
DELETE FROM ref.permission WHERE code IN ('materials.material.read', 'materials.material.write');
REVOKE ALL ON SCHEMA materials FROM erp_relay;
DROP SCHEMA materials CASCADE;
