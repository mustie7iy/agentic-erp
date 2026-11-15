DELETE FROM identity.sod_rule WHERE permission_a IN ('leases.contract.read', 'leases.contract.write')
  OR permission_b IN ('leases.contract.read', 'leases.contract.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('leases.contract.read', 'leases.contract.write');
DELETE FROM ref.permission WHERE code IN ('leases.contract.read', 'leases.contract.write');
REVOKE ALL ON SCHEMA leases FROM erp_relay;
DROP SCHEMA leases CASCADE;
