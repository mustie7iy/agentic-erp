-- Partial payments (T75f; ADR-0063 §6 As built; 03-data-model §3.26): an
-- incoming payment applied to a customer's items may leave a residual
-- item on an item it pays in part, and may write off a difference within
-- the company's payment tolerance (SAP OBA3) to the two new account
-- determination keys. The residual item is a debit line of the payment's
-- clearing entry on the item's account with the customer: residual_line_no
-- names it, residual_amount is what stays open, in the payment's currency,
-- and residual_reason (with an optional free text) says why. difference is
-- what the items applied, net of their residuals, exceed the receipt by:
-- positive an underpayment written off as an expense, negative an
-- overpayment kept as revenue, zero for an exact application.
--
-- Expand only (06 §6.4): nullable or defaulted columns and a wider key
-- check, which the previous version neither reads nor writes; the check
-- is added NOT VALID, which reads no row under the lock, every row
-- satisfying the narrower one, and 000351 validates it.
ALTER TABLE receivables.incoming_payment
  ADD COLUMN difference bigint NOT NULL DEFAULT 0;

ALTER TABLE receivables.incoming_payment_item
  ADD COLUMN residual_amount bigint,
  ADD COLUMN residual_line_no integer,
  ADD COLUMN residual_reason text CONSTRAINT incoming_payment_item_residual_reason_length CHECK (length(residual_reason) BETWEEN 1 AND 40),
  ADD COLUMN residual_reason_text text CONSTRAINT incoming_payment_item_residual_text_length CHECK (length(residual_reason_text) BETWEEN 1 AND 200),
  -- A residual is the amount, its line and its reason together, on an
  -- item the customer owed (a debit), more than zero and less than it.
  ADD CONSTRAINT incoming_payment_item_residual CHECK (
    (residual_amount IS NULL) = (residual_line_no IS NULL) AND (residual_amount IS NULL) = (residual_reason IS NULL)
    AND (residual_reason_text IS NULL OR residual_amount IS NOT NULL)
    AND (residual_amount IS NULL OR (residual_amount > 0 AND residual_amount < amount AND residual_line_no > 0)));

ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue')) NOT VALID;
