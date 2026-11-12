DELETE FROM identity.sod_rule WHERE permission_a IN ('sales.quotation.read', 'sales.quotation.write')
  OR permission_b IN ('sales.quotation.read', 'sales.quotation.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('sales.quotation.read', 'sales.quotation.write');
DELETE FROM ref.permission WHERE code IN ('sales.quotation.read', 'sales.quotation.write');
REVOKE ALL ON SCHEMA sales FROM erp_relay;
DROP SCHEMA sales CASCADE;
