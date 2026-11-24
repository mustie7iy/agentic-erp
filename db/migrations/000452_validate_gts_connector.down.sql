-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000451's state by adding them back NOT VALID.
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider', 'wms', 'tms', 'gts')) NOT VALID;
ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check
    CHECK (kind IN ('outbound_delivery', 'goods_issue', 'inbound_receipt', 'goods_receipt', 'transport_request', 'shipment', 'screening_request',
      'screening_result')) NOT VALID,
  DROP CONSTRAINT wms_message_direction,
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind IN ('outbound_delivery', 'inbound_receipt', 'transport_request',
      'screening_request'))
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed'))
    AND (status <> 'recorded' OR kind IN ('shipment', 'screening_result')) AND (status <> 'posted' OR kind NOT IN ('shipment', 'screening_result'))) NOT VALID;
ALTER TABLE shipping.outbound_delivery
  DROP CONSTRAINT outbound_delivery_trade_compliance_status_check,
  ADD CONSTRAINT outbound_delivery_trade_compliance_status_check CHECK (trade_compliance_status IN ('pending', 'released', 'blocked')) NOT VALID,
  DROP CONSTRAINT outbound_delivery_trade_compliance_reason_check,
  ADD CONSTRAINT outbound_delivery_trade_compliance_reason_check CHECK (length(trade_compliance_reason) BETWEEN 1 AND 40) NOT VALID,
  DROP CONSTRAINT outbound_delivery_trade_compliance,
  ADD CONSTRAINT outbound_delivery_trade_compliance CHECK (
    (trade_compliance_status IS NOT NULL OR (trade_compliance_screened_at IS NULL AND trade_compliance_released_by IS NULL))
    AND (trade_compliance_reason IS NULL OR trade_compliance_screened_at IS NOT NULL)
    AND (trade_compliance_released_by IS NULL) = (trade_compliance_released_at IS NULL)
    AND (trade_compliance_released_by IS NULL) = (trade_compliance_released_by_name IS NULL)
    AND (trade_compliance_released_by IS NULL OR trade_compliance_status = 'released')
    AND (trade_compliance_status <> 'pending' OR trade_compliance_screened_at IS NULL)
    AND (trade_compliance_status <> 'blocked' OR trade_compliance_screened_at IS NOT NULL)
    AND (trade_compliance_status <> 'released' OR trade_compliance_screened_at IS NOT NULL OR trade_compliance_released_by IS NOT NULL)) NOT VALID;
