DELETE FROM identity.sod_rule WHERE 'ledger.carry_forward.run' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'ledger.carry_forward.run';
DELETE FROM ref.permission WHERE code = 'ledger.carry_forward.run';
DROP TABLE ledger.carry_forward_period;
DROP TABLE ledger.carry_forward_balance;
DROP FUNCTION ledger.check_carry_forward_result();
DROP TABLE ledger.carry_forward_run;
DROP FUNCTION ledger.check_carry_forward_run();
DELETE FROM ledger.account_determination WHERE key = 'retained_earnings';
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment'));
DROP TRIGGER period_status_opened_count ON ledger.period_status;
DROP FUNCTION ledger.count_period_openings();
ALTER TABLE ledger.period_status DROP COLUMN opened_count;
