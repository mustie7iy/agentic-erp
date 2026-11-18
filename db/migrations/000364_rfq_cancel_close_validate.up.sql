-- Validates what 000363 added NOT VALID (06 §6.4).
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_status_check;
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_closing_reason_check;
