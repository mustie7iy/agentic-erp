REVOKE SELECT (tenant_id, id, operation_id, status) ON integration.migration_run FROM erp_worker;
REVOKE USAGE ON SCHEMA integration FROM erp_worker;
