-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000231's state by adding it back NOT VALID.
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing', 'treasury_export',
      'einvoice_provider')) NOT VALID;
