DELETE FROM identity.sod_rule WHERE 'ledger.period.lock' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'ledger.period.lock';
DELETE FROM ref.permission WHERE code = 'ledger.period.lock';
DROP TRIGGER period_status_transition ON ledger.period_status;
DROP FUNCTION ledger.check_period_transition();
