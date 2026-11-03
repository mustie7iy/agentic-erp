DELETE FROM identity.sod_rule WHERE permission_a = 'closing.close.read' OR permission_b = 'closing.close.read';
DELETE FROM identity.role_permission WHERE permission_code = 'closing.close.read';
DELETE FROM ref.permission WHERE code = 'closing.close.read';
