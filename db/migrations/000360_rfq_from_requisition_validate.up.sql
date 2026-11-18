-- Validates what 000359 added NOT VALID (06 §6.4).
ALTER TABLE procurement.requisition VALIDATE CONSTRAINT requisition_status_check;
ALTER TABLE procurement.rfq VALIDATE CONSTRAINT rfq_requisition_fkey;
