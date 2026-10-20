-- Validates what 000102 added NOT VALID (06 §6.4): every file before it is
-- in a status the new check lists, with no transmission columns set.
ALTER TABLE payments.bank_file VALIDATE CONSTRAINT bank_file_status_check;
ALTER TABLE payments.bank_file VALIDATE CONSTRAINT bank_file_transmission;
