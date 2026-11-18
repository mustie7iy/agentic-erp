DELETE FROM identity.sod_rule WHERE permission_a IN ('inventory.movement.post', 'inventory.stock.read')
  OR permission_b IN ('inventory.movement.post', 'inventory.stock.read');
DELETE FROM identity.role_permission WHERE permission_code IN ('inventory.movement.post', 'inventory.stock.read');
DELETE FROM ref.permission WHERE code IN ('inventory.movement.post', 'inventory.stock.read');
REVOKE ALL ON SCHEMA inventory FROM erp_relay;
DROP SCHEMA inventory CASCADE;
