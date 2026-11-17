-- Validates what 000348 added NOT VALID (06 §6.4).
ALTER TABLE payments.payment_exclusion VALIDATE CONSTRAINT payment_exclusion_reason_code_check;
ALTER TABLE payments.method_exclusion VALIDATE CONSTRAINT method_exclusion_reason_code_check;
ALTER TABLE payments.request_exclusion VALIDATE CONSTRAINT request_exclusion_reason_code_check;
