-- The control plane (T11): operators list the cell's tenants and change
-- their status through the admin listener. Listing crosses tenants, which
-- the application role cannot do; erp_control is the separate, audited
-- maintenance role of ADR-0003, and it only reads. A status change runs in
-- the tenant's own scope as erp_app, so the tenant's audit trail and outbox
-- record it like any other change.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_control') THEN CREATE ROLE erp_control NOLOGIN BYPASSRLS; END IF;
END $$;
GRANT USAGE ON SCHEMA tenancy, platform TO erp_control;
GRANT SELECT ON tenancy.tenant TO erp_control;
GRANT SELECT ON platform.idempotency_key TO erp_control;

-- The Idempotency-Key of a provisioning request is stored with the tenant it
-- created (there is no tenant before it). Control keys carry a prefix, are
-- unique across tenants and are found by key alone.
CREATE UNIQUE INDEX idempotency_key_control ON platform.idempotency_key (key) WHERE key LIKE 'control|%';
