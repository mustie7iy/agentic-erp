-- Validates the checks 000390 added or widened NOT VALID (06 §6.4).
ALTER TABLE procurement.purchase_order_item VALIDATE CONSTRAINT purchase_order_item_stock;
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_movement_type_check;
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_reversal_check;
ALTER TABLE inventory.material_document VALIDATE CONSTRAINT material_document_reference;
