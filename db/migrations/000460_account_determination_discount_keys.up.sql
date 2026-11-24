-- Two account determination keys of their own (ADR-0063 As built, cash
-- discount granted; ADR-0061 As built batch 92; 03-data-model §3.7):
-- cash_discount_granted, the expense a cash discount granted to a customer
-- is debited to (SAP SKT), which shared payment_difference_expense with
-- the payment differences written off; and supplier_rebate, the account a
-- volume discount's credit memo naming no order is credited to (the
-- supplier rebates received), which shared cash_discount with the cash
-- discounts payments take from suppliers (SAP SKE). A company that does
-- not set one falls back to the key it shared
-- (ledger.Accounts.Determination), so nothing here is filled in.
--
-- Expand only (06 §6.4): a wider key check, which the previous version
-- never writes outside of; added NOT VALID, which reads no row under the
-- lock, every row satisfying the narrower one, and 000461 validates it.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue',
                 'cash_discount_granted', 'supplier_rebate')
         OR key ~ '^inventory_(stock|receipt|initial_entry|consumption|scrapping|price_difference|revaluation|difference|cost_of_goods_sold):[0-9A-Z]{1,4}$') NOT VALID;
