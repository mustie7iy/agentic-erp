DELETE FROM identity.sod_rule WHERE permission_a = 'inventory.count_plan.write' OR permission_b = 'inventory.count_plan.write';
DELETE FROM identity.role_permission WHERE permission_code = 'inventory.count_plan.write';
DELETE FROM ref.permission WHERE code = 'inventory.count_plan.write';
DROP TABLE inventory.cycle_count_material;
DROP TABLE inventory.cycle_count_indicator;
