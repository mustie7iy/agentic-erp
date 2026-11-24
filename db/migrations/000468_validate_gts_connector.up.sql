-- Validates the checks 000467 added or widened NOT VALID (06 §6.4),
-- scanning each table without blocking writes.
ALTER TABLE integration.connector VALIDATE CONSTRAINT connector_kind_check;
ALTER TABLE integration.wms_message VALIDATE CONSTRAINT wms_message_kind_check;
ALTER TABLE integration.wms_message VALIDATE CONSTRAINT wms_message_direction;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_trade_compliance_status_check;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_trade_compliance_reason_check;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_trade_compliance;
