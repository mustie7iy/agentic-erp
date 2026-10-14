-- Rows with the new reasons may exist; the narrower check comes back NOT
-- VALID so it does not refuse them, and refuses new ones.
ALTER TABLE payments.payment_exclusion DROP CONSTRAINT payment_exclusion_reason_code_check;
ALTER TABLE payments.payment_exclusion ADD CONSTRAINT payment_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance')) NOT VALID;
