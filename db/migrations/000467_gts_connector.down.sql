-- GTS connectors, their messages and the deliveries screened, which the
-- previous version cannot hold, are refused rather than dropped (06 §6.4:
-- a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM integration.connector WHERE kind = 'gts')
     OR EXISTS (SELECT FROM integration.wms_message WHERE kind IN ('screening_request', 'screening_result'))
     OR EXISTS (SELECT FROM shipping.outbound_delivery WHERE trade_compliance_status IS NOT NULL) THEN
    RAISE EXCEPTION 'gts connectors or screened deliveries exist; the previous version cannot hold them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
DELETE FROM identity.sod_rule WHERE permission_a = 'shipping.trade_compliance.release' OR permission_b = 'shipping.trade_compliance.release';
DELETE FROM identity.role_permission WHERE permission_code = 'shipping.trade_compliance.release';
DELETE FROM ref.permission WHERE code = 'shipping.trade_compliance.release';
REVOKE UPDATE (trade_compliance_status, trade_compliance_reason, trade_compliance_screened_at, trade_compliance_released_by,
  trade_compliance_released_by_name, trade_compliance_released_at) ON shipping.outbound_delivery FROM erp_app;
ALTER TABLE shipping.outbound_delivery
  DROP COLUMN trade_compliance_status,
  DROP COLUMN trade_compliance_reason,
  DROP COLUMN trade_compliance_screened_at,
  DROP COLUMN trade_compliance_released_by,
  DROP COLUMN trade_compliance_released_by_name,
  DROP COLUMN trade_compliance_released_at;
ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check
    CHECK (kind IN ('outbound_delivery', 'goods_issue', 'inbound_receipt', 'goods_receipt', 'transport_request', 'shipment')),
  DROP CONSTRAINT wms_message_direction,
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind IN ('outbound_delivery', 'inbound_receipt', 'transport_request'))
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed'))
    AND (status <> 'recorded' OR kind = 'shipment') AND (status <> 'posted' OR kind <> 'shipment'));
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider', 'wms', 'tms'));
