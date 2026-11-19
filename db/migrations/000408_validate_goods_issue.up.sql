-- Validates the checks 000407 widened NOT VALID (06 §6.4).
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_movement_type_check;
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_reversal_check;
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_reference;
ALTER TABLE ledger.account_determination VALIDATE CONSTRAINT account_determination_key_check;
