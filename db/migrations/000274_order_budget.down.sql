DELETE FROM identity.sod_rule WHERE permission_a = 'controlling.order.budget' OR permission_b = 'controlling.order.budget';
DELETE FROM identity.role_permission WHERE permission_code = 'controlling.order.budget';
DELETE FROM ref.permission WHERE code = 'controlling.order.budget';
DROP TABLE controlling.order_budget;
