DELETE FROM identity.sod_rule WHERE permission_a = 'tax.registration.read' OR permission_b = 'tax.registration.read';
DELETE FROM identity.role_permission WHERE permission_code = 'tax.registration.read';
DELETE FROM ref.permission WHERE code = 'tax.registration.read';
DROP SCHEMA tax CASCADE;
