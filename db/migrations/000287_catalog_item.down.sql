DELETE FROM identity.sod_rule WHERE permission_a IN ('procurement.catalog.read', 'procurement.catalog.manage')
  OR permission_b IN ('procurement.catalog.read', 'procurement.catalog.manage');
DELETE FROM identity.role_permission WHERE permission_code IN ('procurement.catalog.read', 'procurement.catalog.manage');
DELETE FROM ref.permission WHERE code IN ('procurement.catalog.read', 'procurement.catalog.manage');
DROP TABLE procurement.catalog_item;
