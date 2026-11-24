-- TMS connectors, their messages and the shipments recorded on deliveries,
-- which the previous version cannot hold, are refused rather than dropped
-- (06 §6.4: a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM integration.connector WHERE kind = 'tms')
     OR EXISTS (SELECT FROM integration.wms_message WHERE kind IN ('transport_request', 'shipment'))
     OR EXISTS (SELECT FROM shipping.outbound_delivery WHERE shipment_number IS NOT NULL) THEN
    RAISE EXCEPTION 'tms connectors or recorded shipments exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
CREATE OR REPLACE FUNCTION shipping.outbound_delivery_open() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'created' THEN
    RAISE EXCEPTION 'outbound delivery %: its goods issue is posted, so it changes no more', OLD.number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outbound_delivery_issued_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE shipping.outbound_delivery
  DROP COLUMN shipment_number,
  DROP COLUMN carrier_code,
  DROP COLUMN freight_amount,
  DROP COLUMN freight_currency,
  DROP COLUMN shipment_recorded_at;
ALTER TABLE integration.wms_message
  DROP CONSTRAINT wms_message_kind_check,
  ADD CONSTRAINT wms_message_kind_check CHECK (kind IN ('outbound_delivery', 'goods_issue', 'inbound_receipt', 'goods_receipt')),
  DROP CONSTRAINT wms_message_status_check,
  ADD CONSTRAINT wms_message_status_check CHECK (status IN ('pending', 'sent', 'failed', 'posted', 'refused')),
  DROP CONSTRAINT wms_message_direction,
  ADD CONSTRAINT wms_message_direction CHECK ((direction = 'outbound') = (kind IN ('outbound_delivery', 'inbound_receipt'))
    AND (direction = 'outbound') = (status IN ('pending', 'sent', 'failed')));
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider', 'wms'));
