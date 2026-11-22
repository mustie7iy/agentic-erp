-- WMS connectors and their messages the previous version cannot hold are
-- refused rather than dropped (06 §6.4: a down migration loses no data).
DO $$
BEGIN
  IF EXISTS (SELECT FROM integration.wms_message) OR EXISTS (SELECT FROM integration.connector WHERE kind = 'wms') THEN
    RAISE EXCEPTION 'wms connectors exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE integration.wms_message;
DROP FUNCTION integration.wms_message_kept();
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider'));
