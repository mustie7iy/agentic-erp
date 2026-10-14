-- Validates the exclusion reasons check 000035 added NOT VALID (06 §6.4);
-- every row meets it, the old check being narrower.
ALTER TABLE payments.payment_exclusion VALIDATE CONSTRAINT payment_exclusion_reason_code_check;
