-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000227's state by adding the checks back NOT VALID.
ALTER TABLE ledger.period_status
  DROP CONSTRAINT period_status_opened_count,
  ADD CONSTRAINT period_status_opened_count CHECK (opened_count >= 0) NOT VALID;
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings')) NOT VALID;
