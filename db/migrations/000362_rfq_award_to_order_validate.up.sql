-- Validates what 000361 added NOT VALID (06 §6.4).
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_purchase_order_fkey;
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_award_reason_check;
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_awarded_to_one_document;
