-- A release drops a payment that can never post (F6, 03-data-model §3.10):
-- an item it settles was reversed or is no longer open since the proposal.
-- The payment is excluded with the reason, so the exclusion reasons gain
-- item_reversed and item_not_open. The wider check is added NOT VALID (no
-- scan under the lock, 06 §6.4) and validated by 000036.
ALTER TABLE payments.payment_exclusion DROP CONSTRAINT payment_exclusion_reason_code_check;
ALTER TABLE payments.payment_exclusion ADD CONSTRAINT payment_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance', 'item_reversed', 'item_not_open')) NOT VALID;
