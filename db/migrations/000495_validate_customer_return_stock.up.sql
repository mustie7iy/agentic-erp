-- Validates the checks 000494 widened NOT VALID (06 §6.4).
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_movement_type_check;
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_reference;
