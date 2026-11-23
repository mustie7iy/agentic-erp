-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000449's state by adding them back NOT VALID.
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider', 'wms', 'tms')) NOT VALID;
ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check
    CHECK (kind IN ('outbound_delivery', 'goods_issue', 'inbound_receipt', 'goods_receipt', 'transport_request', 'shipment')) NOT VALID,
  DROP CONSTRAINT wms_message_status_check,
  ADD CONSTRAINT wms_message_status_check CHECK (status IN ('pending', 'sent', 'failed', 'posted', 'refused', 'recorded')) NOT VALID,
  DROP CONSTRAINT wms_message_direction,
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind IN ('outbound_delivery', 'inbound_receipt', 'transport_request'))
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed'))
    AND (status <> 'recorded' OR kind = 'shipment') AND (status <> 'posted' OR kind <> 'shipment')) NOT VALID;
ALTER TABLE shipping.outbound_delivery
  DROP CONSTRAINT outbound_delivery_shipment_number_check,
  ADD CONSTRAINT outbound_delivery_shipment_number_check CHECK (length(shipment_number) BETWEEN 1 AND 40) NOT VALID,
  DROP CONSTRAINT outbound_delivery_carrier_code_check,
  ADD CONSTRAINT outbound_delivery_carrier_code_check CHECK (length(carrier_code) BETWEEN 1 AND 40) NOT VALID,
  DROP CONSTRAINT outbound_delivery_freight_amount_check,
  ADD CONSTRAINT outbound_delivery_freight_amount_check CHECK (freight_amount >= 0) NOT VALID,
  DROP CONSTRAINT outbound_delivery_freight_currency_fkey,
  ADD CONSTRAINT outbound_delivery_freight_currency_fkey FOREIGN KEY (freight_currency) REFERENCES ref.currency NOT VALID,
  DROP CONSTRAINT outbound_delivery_shipment,
  ADD CONSTRAINT outbound_delivery_shipment CHECK ((shipment_number IS NULL) = (carrier_code IS NULL) AND (shipment_number IS NULL) = (freight_amount IS NULL)
    AND (shipment_number IS NULL) = (freight_currency IS NULL) AND (shipment_number IS NULL) = (shipment_recorded_at IS NULL)) NOT VALID;
