-- The revert is refused while a company names an account for either key:
-- the previous version cannot hold the row, and dropping it would post
-- that company's discounts and rebates to the key it shared from then on
-- (06 §6.4: a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM ledger.account_determination WHERE key IN ('cash_discount_granted', 'supplier_rebate')) THEN
    RAISE EXCEPTION 'companies name accounts for cash discounts granted or supplier rebates; the previous version cannot hold them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
-- The check goes back to 000411's state: validated.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue')
         OR key ~ '^inventory_(stock|receipt|initial_entry|consumption|scrapping|price_difference|revaluation|difference|cost_of_goods_sold):[0-9A-Z]{1,4}$');
