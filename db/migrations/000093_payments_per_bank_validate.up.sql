-- Validates what 000092 added NOT VALID (06 §6.4): the new columns are null
-- on every row before it, and house_bank_active_account holds on the
-- backfilled rows.
ALTER TABLE payments.payment VALIDATE CONSTRAINT payment_house_bank;
ALTER TABLE payments.payment VALIDATE CONSTRAINT payment_method_of_bank;
ALTER TABLE payments.bank_file VALIDATE CONSTRAINT bank_file_house_bank;
ALTER TABLE payments.bank_file VALIDATE CONSTRAINT bank_file_method_of_bank;
ALTER TABLE payments.payment_exclusion VALIDATE CONSTRAINT payment_exclusion_reason_code_check;
ALTER TABLE payments.request_exclusion VALIDATE CONSTRAINT request_exclusion_reason_code_check;
ALTER TABLE payments.house_bank VALIDATE CONSTRAINT house_bank_active_account;
