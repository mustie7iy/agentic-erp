-- Runs without a house bank, and the payments and files of one, cannot be
-- kept by the previous schema; the down refuses rather than drop them.
-- Exclusions with no_house_bank may exist, so the narrower checks come back
-- NOT VALID: they refuse new rows, not the old.
SET LOCAL row_security = off;
DO $$
BEGIN
  IF EXISTS (SELECT FROM payments.payment_run WHERE house_bank_id IS NULL) THEN
    RAISE EXCEPTION 'payment runs without a house bank exist';
  END IF;
END $$;
ALTER TABLE payments.house_bank DROP CONSTRAINT house_bank_active_account, DROP COLUMN active_gl_account_id;
ALTER TABLE payments.request_exclusion DROP CONSTRAINT request_exclusion_reason_code_check;
ALTER TABLE payments.request_exclusion ADD CONSTRAINT request_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'tax_code_invalid')) NOT VALID;
ALTER TABLE payments.payment_exclusion DROP CONSTRAINT payment_exclusion_reason_code_check;
ALTER TABLE payments.payment_exclusion ADD CONSTRAINT payment_exclusion_reason_code_check CHECK (reason_code IN ('supplier_blocked', 'payment_blocked',
  'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance', 'item_reversed', 'item_not_open')) NOT VALID;
ALTER TABLE payments.bank_file DROP COLUMN payment_method, DROP COLUMN house_bank_id;
ALTER TABLE payments.payment DROP COLUMN payment_method, DROP COLUMN house_bank_id;
ALTER TABLE payments.payment_run ALTER COLUMN house_bank_id SET NOT NULL;
