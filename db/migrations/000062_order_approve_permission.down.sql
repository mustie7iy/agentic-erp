DELETE FROM identity.sod_rule WHERE 'procurement.order.approve' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'procurement.order.approve';
DELETE FROM ref.permission WHERE code = 'procurement.order.approve';
