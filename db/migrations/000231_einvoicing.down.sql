DELETE FROM identity.sod_rule WHERE permission_a IN ('einvoicing.einvoice.read', 'einvoicing.einvoice.manage')
  OR permission_b IN ('einvoicing.einvoice.read', 'einvoicing.einvoice.manage');
DELETE FROM identity.role_permission WHERE permission_code IN ('einvoicing.einvoice.read', 'einvoicing.einvoice.manage');
DELETE FROM ref.permission WHERE code IN ('einvoicing.einvoice.read', 'einvoicing.einvoice.manage');
REVOKE ALL ON SCHEMA einvoicing FROM erp_relay;
DROP SCHEMA einvoicing CASCADE;
-- Nothing but the e-invoices dropped above refers to an einvoice_provider
-- connector.
DELETE FROM integration.connector WHERE kind = 'einvoice_provider';
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export'));
