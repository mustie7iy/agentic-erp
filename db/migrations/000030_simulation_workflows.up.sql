-- Simulations as workflows (T28, ADR-0012): the dispatcher names a change
-- request's simulation workflow after the request's number and reads the
-- operation's request to find it, across tenants as the worker role.
GRANT USAGE ON SCHEMA config TO erp_worker;
GRANT SELECT (tenant_id, id, number) ON config.change_request TO erp_worker;
GRANT SELECT (request) ON platform.operation TO erp_worker;
