-- Validates the checks 000407 widened NOT VALID (06 §6.4): the material
-- document's movement types and the account determination's keys.
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_movement_type_check;
ALTER TABLE ledger.account_determination VALIDATE CONSTRAINT account_determination_key_check;
