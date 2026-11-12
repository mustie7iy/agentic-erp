DELETE FROM identity.sod_rule WHERE permission_a IN ('collections.dunning.read', 'collections.dunning.run')
  OR permission_b IN ('collections.dunning.read', 'collections.dunning.run');
DELETE FROM identity.role_permission WHERE permission_code IN ('collections.dunning.read', 'collections.dunning.run');
DELETE FROM ref.permission WHERE code IN ('collections.dunning.read', 'collections.dunning.run');
REVOKE ALL ON SCHEMA collections FROM erp_relay;
DROP SCHEMA collections CASCADE;
