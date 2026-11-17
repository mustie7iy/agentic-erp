-- The revert is refused while a payment left a residual item or wrote a
-- difference off: its clearing entry is posted in the ledger, and
-- dropping the columns would lose which line is the residual and why, and
-- what was written off. The keys leave the account determination, whose
-- restored check refuses them.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.incoming_payment_item WHERE residual_amount IS NOT NULL)
     OR EXISTS (SELECT FROM receivables.incoming_payment WHERE difference <> 0) THEN
    RAISE EXCEPTION 'receivables holds incoming payments with residual items or payment differences: reverting would lose them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM ledger.account_determination WHERE key IN ('payment_difference_expense', 'payment_difference_revenue');
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation'));
ALTER TABLE receivables.incoming_payment_item
  DROP CONSTRAINT incoming_payment_item_residual,
  DROP COLUMN residual_reason_text,
  DROP COLUMN residual_reason,
  DROP COLUMN residual_line_no,
  DROP COLUMN residual_amount;
ALTER TABLE receivables.incoming_payment DROP COLUMN difference;
