-- The tenant of the transaction, or an error when none is set (F5b): for
-- keys that must never be NULL. platform.current_tenant() is NULL when
-- app.tenant_id is unset, which matches no rows under RLS; an advisory lock
-- keyed by it would instead be skipped silently (the lock functions are
-- strict), so the ledger's entry locks take the tenant from here.
CREATE FUNCTION platform.require_tenant() RETURNS uuid
LANGUAGE plpgsql STABLE AS $$
DECLARE
  t uuid := platform.current_tenant();
BEGIN
  IF t IS NULL THEN
    RAISE EXCEPTION 'no tenant scope: app.tenant_id is not set' USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN t;
END $$;
ALTER FUNCTION platform.require_tenant() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION platform.require_tenant() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.require_tenant() TO erp_app;
