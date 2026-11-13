DELETE FROM identity.sod_rule WHERE permission_a = 'records.privacy.read' OR permission_b = 'records.privacy.read';
DELETE FROM identity.role_permission WHERE permission_code = 'records.privacy.read';
DELETE FROM ref.permission WHERE code = 'records.privacy.read';
