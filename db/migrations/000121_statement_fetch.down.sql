-- No earlier migration grants erp_worker these columns, so revoking them
-- restores 000120's grants exactly.
REVOKE SELECT (tenant_id, id, kind, status, settings) ON integration.connector FROM erp_worker;
REVOKE SELECT (tenant_id, id, connector_id, status) ON payments.house_bank FROM erp_worker;
DROP TABLE payments.statement_fetch;
