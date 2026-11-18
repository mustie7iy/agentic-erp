-- Validates what 000371 added NOT VALID (06 §6.4).
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_status_check;
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_awarded_to_one_document;
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_award_reason_check;
