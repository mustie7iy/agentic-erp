DELETE FROM identity.sod_rule WHERE permission_a = 'closing.close.manage' OR permission_b = 'closing.close.manage';
DELETE FROM identity.role_permission WHERE permission_code = 'closing.close.manage';
DELETE FROM ref.permission WHERE code = 'closing.close.manage';
REVOKE ALL ON SCHEMA closing FROM erp_relay;
DROP SCHEMA closing CASCADE;
