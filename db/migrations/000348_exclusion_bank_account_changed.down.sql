-- Exclusions with bank_account_changed may exist, so the narrower checks
-- come back NOT VALID: they refuse new rows, not the old (as 000093's down).
ALTER TABLE payments.request_exclusion DROP CONSTRAINT request_exclusion_reason_code_check;
ALTER TABLE payments.request_exclusion ADD CONSTRAINT request_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'tax_code_invalid', 'no_house_bank')) NOT VALID;
ALTER TABLE payments.method_exclusion DROP CONSTRAINT method_exclusion_reason_code_check;
ALTER TABLE payments.method_exclusion ADD CONSTRAINT method_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance', 'item_reversed', 'item_not_open', 'no_house_bank',
  'method_not_active', 'method_not_allowed', 'fixed_method_in_bank_run')) NOT VALID;
ALTER TABLE payments.payment_exclusion DROP CONSTRAINT payment_exclusion_reason_code_check;
ALTER TABLE payments.payment_exclusion ADD CONSTRAINT payment_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance', 'item_reversed', 'item_not_open', 'no_house_bank')) NOT VALID;
