-- Validates the checks 000453 widened, replaced or added NOT VALID (06
-- §6.4), scanning the table without blocking writes.
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_status_check;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_goods_issue;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_reversal_material_document_number_check;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_goods_issue_reversal;
