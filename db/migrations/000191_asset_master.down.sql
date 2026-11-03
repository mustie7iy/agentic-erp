DELETE FROM identity.sod_rule WHERE permission_a = 'assets.asset.change' OR permission_b = 'assets.asset.change';
DELETE FROM identity.role_permission WHERE permission_code = 'assets.asset.change';
DELETE FROM ref.permission WHERE code = 'assets.asset.change';
REVOKE ALL ON SCHEMA assets FROM erp_relay;
DROP SCHEMA assets CASCADE;
