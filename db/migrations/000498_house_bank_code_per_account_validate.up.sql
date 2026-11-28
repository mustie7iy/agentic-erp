-- Validates the check 000497 widened NOT VALID (06 §6.4), scanning the
-- table without blocking writes.
ALTER TABLE payments.house_bank VALIDATE CONSTRAINT house_bank_code_check;
