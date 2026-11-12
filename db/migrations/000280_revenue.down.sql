DELETE FROM identity.sod_rule WHERE permission_a IN ('revenue.contract.read', 'revenue.contract.write')
  OR permission_b IN ('revenue.contract.read', 'revenue.contract.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('revenue.contract.read', 'revenue.contract.write');
DELETE FROM ref.permission WHERE code IN ('revenue.contract.read', 'revenue.contract.write');
REVOKE ALL ON SCHEMA revenue FROM erp_relay;
DROP SCHEMA revenue CASCADE;
