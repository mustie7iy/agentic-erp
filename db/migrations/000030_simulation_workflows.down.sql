REVOKE SELECT (request) ON platform.operation FROM erp_worker;
REVOKE SELECT (tenant_id, id, number) ON config.change_request FROM erp_worker;
REVOKE USAGE ON SCHEMA config FROM erp_worker;
