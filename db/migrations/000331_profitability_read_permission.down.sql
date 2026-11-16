DELETE FROM identity.sod_rule WHERE permission_a = 'controlling.profitability.read' OR permission_b = 'controlling.profitability.read';
DELETE FROM identity.role_permission WHERE permission_code = 'controlling.profitability.read';
DELETE FROM ref.permission WHERE code = 'controlling.profitability.read';
