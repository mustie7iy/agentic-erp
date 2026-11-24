-- Validates the checks and foreign key 000462 added or widened NOT VALID
-- (06 §6.4), scanning each table without blocking writes.
ALTER TABLE integration.connector VALIDATE CONSTRAINT connector_kind_check;
ALTER TABLE integration.wms_message VALIDATE CONSTRAINT wms_message_kind_check;
ALTER TABLE integration.wms_message VALIDATE CONSTRAINT wms_message_status_check;
ALTER TABLE integration.wms_message VALIDATE CONSTRAINT wms_message_direction;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_shipment_number_check;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_carrier_code_check;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_freight_amount_check;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_freight_currency_fkey;
ALTER TABLE shipping.outbound_delivery VALIDATE CONSTRAINT outbound_delivery_shipment;
