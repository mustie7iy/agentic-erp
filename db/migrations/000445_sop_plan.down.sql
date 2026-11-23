DELETE FROM identity.sod_rule WHERE permission_a IN ('mrp.sop.read', 'mrp.sop.write') OR permission_b IN ('mrp.sop.read', 'mrp.sop.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('mrp.sop.read', 'mrp.sop.write');
DELETE FROM ref.permission WHERE code IN ('mrp.sop.read', 'mrp.sop.write');
DROP TABLE mrp.sop_plan_line;
DROP TABLE mrp.sop_plan;
DROP FUNCTION mrp.sop_plan_line_fixed();
DROP FUNCTION mrp.sop_plan_fixed();
