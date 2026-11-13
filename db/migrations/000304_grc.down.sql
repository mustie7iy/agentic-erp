DELETE FROM identity.sod_rule WHERE permission_a IN ('grc.control.read', 'grc.control.write', 'grc.control.test')
  OR permission_b IN ('grc.control.read', 'grc.control.write', 'grc.control.test');
DELETE FROM identity.role_permission WHERE permission_code IN ('grc.control.read', 'grc.control.write', 'grc.control.test');
DELETE FROM ref.permission WHERE code IN ('grc.control.read', 'grc.control.write', 'grc.control.test');
REVOKE ALL ON SCHEMA grc FROM erp_relay;
DROP SCHEMA grc CASCADE;
