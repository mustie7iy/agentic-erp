-- A resumed release drops a payment not posted yet whose supplier can no
-- longer be paid as proposed (product owner 2026-11-16, ADR-0038 as built),
-- among the reasons a bank account in force on the run date that is not
-- the one the payment was proposed to: bank_account_changed. A down
-- payment's payment is dropped with an exclusion of its request.
-- NOT VALID here, validated by 000349 (06 §6.4): every row already there
-- holds the wider check.
ALTER TABLE payments.payment_exclusion DROP CONSTRAINT payment_exclusion_reason_code_check;
ALTER TABLE payments.payment_exclusion ADD CONSTRAINT payment_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance', 'item_reversed', 'item_not_open', 'no_house_bank',
  'bank_account_changed')) NOT VALID;
ALTER TABLE payments.method_exclusion DROP CONSTRAINT method_exclusion_reason_code_check;
ALTER TABLE payments.method_exclusion ADD CONSTRAINT method_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance', 'item_reversed', 'item_not_open', 'no_house_bank',
  'method_not_active', 'method_not_allowed', 'fixed_method_in_bank_run', 'bank_account_changed')) NOT VALID;
ALTER TABLE payments.request_exclusion DROP CONSTRAINT request_exclusion_reason_code_check;
ALTER TABLE payments.request_exclusion ADD CONSTRAINT request_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'tax_code_invalid', 'no_house_bank', 'bank_account_changed')) NOT VALID;
