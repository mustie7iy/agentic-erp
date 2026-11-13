DELETE FROM identity.sod_rule WHERE permission_a = 'controlling.margin.read' OR permission_b = 'controlling.margin.read';
DELETE FROM identity.role_permission WHERE permission_code = 'controlling.margin.read';
DELETE FROM ref.permission WHERE code = 'controlling.margin.read';
