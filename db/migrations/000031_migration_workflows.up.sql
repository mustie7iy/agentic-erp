-- Migration and reconciliation runs as workflows (T28, ADR-0012): the
-- dispatcher finds running runs with a queued operation across tenants as
-- the worker role.
GRANT USAGE ON SCHEMA integration TO erp_worker;
GRANT SELECT (tenant_id, id, operation_id, status) ON integration.migration_run TO erp_worker;
