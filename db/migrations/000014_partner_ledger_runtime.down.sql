REVOKE UPDATE ON partner.tax_identifier FROM erp_app;
DROP TRIGGER tax_identifier_touch ON partner.tax_identifier;
ALTER TABLE partner.tax_identifier DROP COLUMN updated_at, DROP COLUMN updated_by, DROP COLUMN version;

REVOKE UPDATE (valid_to, ended_by, ended_at) ON partner.address FROM erp_app;
DROP TRIGGER address_end ON partner.address;
ALTER TABLE partner.address DROP COLUMN ended_at, DROP COLUMN ended_by;

-- Roles are cluster-wide and may hold grants in other databases, so the role
-- itself is kept; only this database's grants are removed.
REVOKE ALL ON ledger.period_balance, ledger.journal_entry, ledger.journal_line FROM erp_projector;
DROP INDEX ledger.period_balance_by_period;
DROP TABLE ledger.balance_projection;
REVOKE USAGE ON SCHEMA ledger FROM erp_projector;

DROP INDEX ledger.journal_entry_by_id;

ALTER TABLE ledger.chart_of_accounts DROP CONSTRAINT chart_of_accounts_code_check;
ALTER TABLE ledger.chart_of_accounts ADD CONSTRAINT chart_of_accounts_code_check CHECK (code ~ '^[A-Z0-9]{1,4}$');

ALTER TABLE config.number_range DROP CONSTRAINT number_range_document_type_check;
ALTER TABLE config.number_range ADD CONSTRAINT number_range_document_type_check CHECK (document_type ~ '^[a-z_]+$');
