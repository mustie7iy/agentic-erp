DELETE FROM identity.sod_rule WHERE permission_a IN ('credit.limit.read', 'credit.limit.write')
  OR permission_b IN ('credit.limit.read', 'credit.limit.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('credit.limit.read', 'credit.limit.write');
DELETE FROM ref.permission WHERE code IN ('credit.limit.read', 'credit.limit.write');
REVOKE ALL ON SCHEMA credit FROM erp_relay;
DROP SCHEMA credit CASCADE;
