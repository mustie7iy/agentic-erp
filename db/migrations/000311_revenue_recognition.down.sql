-- The revert is refused while a recognition run exists: its entries are
-- posted in the ledger, and dropping the runs would lose what the next
-- run takes off, so it would post the contracts' positions again.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM revenue.recognition_run) THEN
    RAISE EXCEPTION 'revenue.recognition_run holds runs whose entries are posted: reverting would lose what later runs take off'
      USING ERRCODE = 'check_violation';
  END IF;
  IF EXISTS (SELECT FROM ledger.account_determination WHERE key IN ('contract_asset', 'contract_liability')) THEN
    RAISE EXCEPTION 'ledger.account_determination holds contract asset or contract liability accounts: remove them from the configuration first'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE 'revenue.recognition.run' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'revenue.recognition.run';
DELETE FROM ref.permission WHERE code = 'revenue.recognition.run';
DROP TABLE revenue.recognition_posting_obligation;
DROP TABLE revenue.recognition_posting;
DROP TABLE revenue.recognition_run;
DROP FUNCTION revenue.check_recognition_run();
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings'));
-- The previous version posts no revenue entry, and the guard above found
-- no run, so no row names the context: the narrower check is restored
-- NOT VALID, as 000280's revert restores its own.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling')) NOT VALID;
