DELETE FROM identity.sod_rule WHERE permission_a IN ('procurement.rfq.read', 'procurement.rfq.manage')
  OR permission_b IN ('procurement.rfq.read', 'procurement.rfq.manage');
DELETE FROM identity.role_permission WHERE permission_code IN ('procurement.rfq.read', 'procurement.rfq.manage');
DELETE FROM ref.permission WHERE code IN ('procurement.rfq.read', 'procurement.rfq.manage');
DROP TABLE procurement.rfq_supplier;
DROP TABLE procurement.rfq_item;
DROP FUNCTION procurement.check_rfq_is_draft();
DROP TABLE procurement.rfq;
DROP FUNCTION procurement.check_rfq();
