DELETE FROM identity.sod_rule WHERE permission_a = 'collections.worklist.read' OR permission_b = 'collections.worklist.read';
DELETE FROM identity.role_permission WHERE permission_code = 'collections.worklist.read';
DELETE FROM ref.permission WHERE code = 'collections.worklist.read';
