DROP FUNCTION platform.erase_tenant(uuid);
CREATE OR REPLACE FUNCTION platform.forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '%.% is insert-only; % is not allowed', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'insufficient_privilege';
END $$;
REVOKE SELECT (id, status, updated_at, exported_at, export_ref, erase_after, erased_at) ON tenancy.tenant FROM erp_worker;
REVOKE USAGE ON SCHEMA tenancy FROM erp_worker;
ALTER TABLE tenancy.tenant
  DROP CONSTRAINT tenant_erased_after_export,
  DROP CONSTRAINT tenant_export_complete,
  DROP COLUMN erased_at,
  DROP COLUMN erase_after,
  DROP COLUMN export_ref,
  DROP COLUMN exported_at;
