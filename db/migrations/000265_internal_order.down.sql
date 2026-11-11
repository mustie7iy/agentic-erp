DELETE FROM identity.sod_rule WHERE permission_a IN ('controlling.order.read', 'controlling.order.write', 'controlling.order.release')
  OR permission_b IN ('controlling.order.read', 'controlling.order.write', 'controlling.order.release');
DELETE FROM identity.role_permission WHERE permission_code IN ('controlling.order.read', 'controlling.order.write', 'controlling.order.release');
DELETE FROM ref.permission WHERE code IN ('controlling.order.read', 'controlling.order.write', 'controlling.order.release');
DROP TABLE controlling.internal_order;
DROP FUNCTION controlling.check_internal_order();
