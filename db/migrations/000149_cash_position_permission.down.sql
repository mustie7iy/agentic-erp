DELETE FROM identity.sod_rule WHERE permission_a = 'cash.position.read' OR permission_b = 'cash.position.read';
DELETE FROM identity.role_permission WHERE permission_code = 'cash.position.read';
DELETE FROM ref.permission WHERE code = 'cash.position.read';
