DELETE FROM identity.sod_rule WHERE permission_a IN ('cash.export.read', 'cash.export.send') OR permission_b IN ('cash.export.read', 'cash.export.send');
DELETE FROM identity.role_permission WHERE permission_code IN ('cash.export.read', 'cash.export.send');
DELETE FROM ref.permission WHERE code IN ('cash.export.read', 'cash.export.send');
REVOKE SELECT (tenant_id, status, next_attempt_at) ON cash.export FROM erp_worker;
DROP TABLE cash.export;
-- A treasury_export connector is not one 000010's check admits, and
-- nothing but the exports dropped above refers to one.
DELETE FROM integration.connector WHERE kind = 'treasury_export';
ALTER TABLE integration.connector
  DROP CONSTRAINT connector_kind_check,
  ADD CONSTRAINT connector_kind_check
    CHECK (kind IN ('sap_odata', 'sap_file', 'bank_sftp', 'bank_ebics', 'edi_as2', 'email', 'tax_engine', 'ticketing'));
