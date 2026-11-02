DELETE FROM identity.sod_rule WHERE permission_a = 'cash.forecast.read' OR permission_b = 'cash.forecast.read';
DELETE FROM identity.role_permission WHERE permission_code = 'cash.forecast.read';
DELETE FROM ref.permission WHERE code = 'cash.forecast.read';
