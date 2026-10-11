DROP INDEX platform.idempotency_key_control;
REVOKE SELECT ON platform.idempotency_key FROM erp_control;
REVOKE SELECT ON tenancy.tenant FROM erp_control;
REVOKE USAGE ON SCHEMA tenancy, platform FROM erp_control;
