DELETE FROM identity.sod_rule WHERE permission_a = 'records.retention.read' OR permission_b = 'records.retention.read';
DELETE FROM identity.role_permission WHERE permission_code = 'records.retention.read';
DELETE FROM ref.permission WHERE code = 'records.retention.read';
