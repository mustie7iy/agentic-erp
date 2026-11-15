DELETE FROM identity.sod_rule WHERE permission_a IN ('collections.dispute.read', 'collections.dispute.write')
  OR permission_b IN ('collections.dispute.read', 'collections.dispute.write');
DELETE FROM identity.role_permission WHERE permission_code IN ('collections.dispute.read', 'collections.dispute.write');
DELETE FROM ref.permission WHERE code IN ('collections.dispute.read', 'collections.dispute.write');
DROP TABLE collections.dispute_case;
DROP FUNCTION collections.check_dispute_case_change();
