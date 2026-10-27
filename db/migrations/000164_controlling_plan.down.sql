DELETE FROM identity.sod_rule WHERE permission_a IN ('controlling.plan.read', 'controlling.plan.write', 'controlling.plan.lock')
  OR permission_b IN ('controlling.plan.read', 'controlling.plan.write', 'controlling.plan.lock');
DELETE FROM identity.role_permission WHERE permission_code IN ('controlling.plan.read', 'controlling.plan.write', 'controlling.plan.lock');
DELETE FROM ref.permission WHERE code IN ('controlling.plan.read', 'controlling.plan.write', 'controlling.plan.lock');
REVOKE ALL ON SCHEMA controlling FROM erp_relay;
DROP SCHEMA controlling CASCADE;
