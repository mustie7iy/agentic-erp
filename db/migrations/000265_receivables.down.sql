DELETE FROM identity.sod_rule WHERE permission_a IN ('receivables.invoice.read', 'receivables.invoice.write', 'receivables.invoice.post')
  OR permission_b IN ('receivables.invoice.read', 'receivables.invoice.write', 'receivables.invoice.post');
DELETE FROM identity.role_permission WHERE permission_code IN ('receivables.invoice.read', 'receivables.invoice.write', 'receivables.invoice.post');
DELETE FROM ref.permission WHERE code IN ('receivables.invoice.read', 'receivables.invoice.write', 'receivables.invoice.post');
REVOKE ALL ON SCHEMA receivables FROM erp_relay;
DROP SCHEMA receivables CASCADE;
