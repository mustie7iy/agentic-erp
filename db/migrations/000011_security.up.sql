-- Ownership, row-level security and grants (ADR-0003, ADR-0006, ADR-0010).
-- Runs last and covers every table created by earlier migrations; later
-- migrations call platform.secure_table() for new tables.

-- Applies tenant isolation and the standard grants to one table.
CREATE FUNCTION platform.secure_table(tbl regclass) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  has_tenant boolean;
  has_version boolean;
  is_insert_only boolean;
  is_effective_dated boolean;
  nm text := tbl::text;
BEGIN
  EXECUTE format('ALTER TABLE %s OWNER TO erp_owner', nm);
  SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = tbl AND attname = 'tenant_id' AND NOT attisdropped) INTO has_tenant;
  SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = tbl AND attname = 'version' AND NOT attisdropped) INTO has_version;
  SELECT EXISTS (SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
                 WHERE t.tgrelid = tbl AND p.proname = 'forbid_mutation') INTO is_insert_only;
  SELECT EXISTS (SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
                 WHERE t.tgrelid = tbl AND p.proname = 'end_validity') INTO is_effective_dated;

  IF has_tenant THEN
    EXECUTE format('ALTER TABLE %s ENABLE ROW LEVEL SECURITY', nm);
    EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', nm);
    EXECUTE format('CREATE POLICY tenant_isolation ON %s USING (tenant_id = platform.current_tenant()) WITH CHECK (tenant_id = platform.current_tenant())', nm);
  END IF;

  EXECUTE format('GRANT SELECT ON %s TO erp_app, erp_readonly', nm);
  EXECUTE format('GRANT INSERT ON %s TO erp_app', nm);
  IF has_version AND NOT is_insert_only THEN
    EXECUTE format('GRANT UPDATE ON %s TO erp_app', nm);
  END IF;
  IF is_effective_dated THEN
    -- Ending an assignment is the only change (platform.end_validity sets ended_at).
    EXECUTE format('GRANT UPDATE (valid_to, ended_by, ended_at) ON %s TO erp_app', nm);
  END IF;
  -- No DELETE for the application anywhere: business data has lifecycles, not deletes.
END $$;

DO $$
DECLARE r record;
BEGIN
  FOR r IN
    SELECT c.oid::regclass AS tbl
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname IN ('platform', 'tenancy', 'identity', 'config', 'partner', 'ledger', 'procurement', 'payables',
                        'payments', 'workflow', 'audit', 'integration', 'agents', 'analytics')
      AND c.relkind IN ('r', 'p') AND NOT c.relispartition
  LOOP
    PERFORM platform.secure_table(r.tbl);
  END LOOP;
END $$;

-- The tenant table is keyed by id rather than tenant_id.
ALTER TABLE tenancy.tenant ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenancy.tenant FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON tenancy.tenant USING (id = platform.current_tenant()) WITH CHECK (id = platform.current_tenant());

-- Mutable columns on otherwise append-style tables.
GRANT UPDATE ON ledger.period_balance, audit.chain_head, platform.idempotency_key TO erp_app;
GRANT UPDATE (is_active) ON payments.payment_item TO erp_app;
DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass AS tbl FROM pg_class c WHERE c.relname = 'outbox' AND c.relkind = 'r' LOOP
    EXECUTE format('GRANT UPDATE (published_at) ON %s TO erp_app', r.tbl);
  END LOOP;
END $$;

-- Reference data is global and read-only.
ALTER TABLE ref.currency OWNER TO erp_owner;
ALTER TABLE ref.country OWNER TO erp_owner;
ALTER TABLE ref.unit_of_measure OWNER TO erp_owner;
ALTER TABLE ref.permission OWNER TO erp_owner;
GRANT SELECT ON ALL TABLES IN SCHEMA ref TO erp_app, erp_readonly;

GRANT USAGE ON SCHEMA ref, platform, tenancy, identity, config, partner, ledger, procurement, payables, payments,
  workflow, audit, integration, agents, analytics TO erp_app, erp_readonly;
-- Constraint checks and triggers run as the table owner, which must reach
-- referenced tables in ref and in its own schemas.
GRANT USAGE ON SCHEMA ref, platform, tenancy, identity, config, partner, ledger, procurement, payables, payments,
  workflow, audit, integration, agents, analytics TO erp_owner;
GRANT SELECT ON ledger.open_item TO erp_app, erp_readonly;
GRANT EXECUTE ON FUNCTION platform.current_tenant(), platform.normalize_ref(text),
  config.next_document_number(uuid, uuid, text, smallint) TO erp_app;
ALTER FUNCTION procurement.check_receipt() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION platform.secure_table(regclass), platform.create_event_tables(text) FROM PUBLIC;
