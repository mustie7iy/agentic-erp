DELETE FROM identity.sod_rule WHERE permission_a = 'closing.accrual.decide' OR permission_b = 'closing.accrual.decide';
DELETE FROM identity.role_permission WHERE permission_code = 'closing.accrual.decide';
DELETE FROM ref.permission WHERE code = 'closing.accrual.decide';
