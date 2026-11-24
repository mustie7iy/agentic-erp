-- A validated check cannot be marked unvalidated again: the down restores
-- 000460's state by adding it back NOT VALID.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue',
                 'cash_discount_granted', 'supplier_rebate')
         OR key ~ '^inventory_(stock|receipt|initial_entry|consumption|scrapping|price_difference|revaluation|difference|cost_of_goods_sold):[0-9A-Z]{1,4}$') NOT VALID;
