-- Validates the checks 000469 added or widened NOT VALID (06 §6.4),
-- scanning each table without blocking writes.
ALTER TABLE assets.movement VALIDATE CONSTRAINT movement_kind_check;
ALTER TABLE assets.movement VALIDATE CONSTRAINT movement_document_kind_check;
ALTER TABLE assets.movement VALIDATE CONSTRAINT movement_impairment;
ALTER TABLE assets.movement VALIDATE CONSTRAINT movement_impairment_document;
ALTER TABLE assets.document_entry VALIDATE CONSTRAINT document_entry_document_kind_check;
