-- Validates the checks 000447 widened NOT VALID on integration.wms_message
-- (06 §6.4), scanning the table without blocking writes.
ALTER TABLE integration.wms_message VALIDATE CONSTRAINT wms_message_kind_check;
ALTER TABLE integration.wms_message VALIDATE CONSTRAINT wms_message_direction;
