DELETE FROM identity.sod_rule WHERE permission_a IN ('mrp.plan.read', 'mrp.plan.write', 'mrp.run.start')
  OR permission_b IN ('mrp.plan.read', 'mrp.plan.write', 'mrp.run.start');
DELETE FROM identity.role_permission WHERE permission_code IN ('mrp.plan.read', 'mrp.plan.write', 'mrp.run.start');
DELETE FROM ref.permission WHERE code IN ('mrp.plan.read', 'mrp.plan.write', 'mrp.run.start');
DROP SCHEMA mrp CASCADE;
