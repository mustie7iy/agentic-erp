DELETE FROM identity.sod_rule WHERE permission_a = 'records.archive.run' OR permission_b = 'records.archive.run';
DELETE FROM identity.role_permission WHERE permission_code = 'records.archive.run';
DELETE FROM ref.permission WHERE code = 'records.archive.run';
REVOKE ALL ON SCHEMA records FROM erp_relay, erp_worker;
DROP SCHEMA records CASCADE;
