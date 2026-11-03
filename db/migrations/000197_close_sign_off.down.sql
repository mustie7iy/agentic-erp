DELETE FROM identity.sod_rule WHERE permission_a = 'closing.close.sign_off' OR permission_b = 'closing.close.sign_off';
DELETE FROM identity.role_permission WHERE permission_code = 'closing.close.sign_off';
DELETE FROM ref.permission WHERE code = 'closing.close.sign_off';
DROP TABLE closing.sign_off;
