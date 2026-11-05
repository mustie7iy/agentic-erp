DELETE FROM identity.sod_rule WHERE permission_a = 'consolidation.read' OR permission_b = 'consolidation.read';
DELETE FROM identity.role_permission WHERE permission_code = 'consolidation.read';
DELETE FROM ref.permission WHERE code = 'consolidation.read';
