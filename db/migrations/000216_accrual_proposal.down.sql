DELETE FROM identity.sod_rule WHERE permission_a = 'closing.accrual.propose' OR permission_b = 'closing.accrual.propose';
DELETE FROM identity.role_permission WHERE permission_code = 'closing.accrual.propose';
DELETE FROM ref.permission WHERE code = 'closing.accrual.propose';
DROP TABLE closing.accrual_evidence;
DROP TABLE closing.accrual_proposal;
DROP FUNCTION closing.check_accrual_proposal();
