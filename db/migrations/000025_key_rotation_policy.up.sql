-- Key rotation policy (T23, ADR-0028): a worker rotates the keys of active
-- tenants whose current key is older than ERP_KEY_ROTATION_AGE. Finding
-- them crosses tenants, which the worker role does; the rotation itself
-- runs in the tenant's scope as erp_app, like an operator's.
GRANT SELECT (tenant_id, status, created_at) ON tenancy.tenant_key TO erp_worker;
