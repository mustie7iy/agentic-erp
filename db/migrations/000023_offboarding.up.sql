-- Offboarding lifecycle (T17, ADR-0021): an offboarded tenant's data is
-- exported to locked object storage, kept for a grace period, then erased
-- from the cell. The tenant row stays as the record that the tenant existed
-- and what became of its data; the audit schema stays as the record of
-- what happened to it.
ALTER TABLE tenancy.tenant
  ADD COLUMN exported_at timestamptz,
  ADD COLUMN export_ref  text,
  ADD COLUMN erase_after timestamptz,
  ADD COLUMN erased_at   timestamptz,
  ADD CONSTRAINT tenant_export_complete CHECK ((exported_at IS NULL) = (export_ref IS NULL) AND (exported_at IS NULL) = (erase_after IS NULL)),
  ADD CONSTRAINT tenant_erased_after_export CHECK (erased_at IS NULL OR exported_at IS NOT NULL);

-- The offboarding worker finds the tenants due and erases them.
GRANT USAGE ON SCHEMA tenancy TO erp_worker;
GRANT SELECT (id, status, updated_at, exported_at, export_ref, erase_after, erased_at) ON tenancy.tenant TO erp_worker;

-- Insert-only tables let the erasure of their tenant through: the setting
-- app.erasing names the tenant being erased for the transaction, and only
-- platform.erase_tenant sets it.
CREATE OR REPLACE FUNCTION platform.forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' AND coalesce(current_setting('app.erasing', true), '') <> '' AND current_setting('app.erasing', true) = (to_jsonb(OLD) ->> 'tenant_id') THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION '%.% is insert-only; % is not allowed', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'insufficient_privilege';
END $$;

-- erase_tenant deletes every row of the tenant from every table that has a
-- tenant_id, except the audit schema, in passes: a table whose rows are
-- still referenced waits for the next pass. It runs as the owner (the only
-- role that may delete), in the tenant's scope so that forced row-level
-- security applies, and returns the rows deleted per table.
CREATE FUNCTION platform.erase_tenant(tenant uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
  todo   text[];
  nxt    text[];
  tbl    text;
  n      bigint;
  counts jsonb := '{}'::jsonb;
  pass   integer := 0;
BEGIN
  PERFORM pg_catalog.set_config('app.tenant_id', tenant::text, true);
  PERFORM pg_catalog.set_config('app.erasing', tenant::text, true);
  SELECT coalesce(array_agg(format('%I.%I', ns.nspname, c.relname) ORDER BY ns.nspname, c.relname), '{}') INTO todo
    FROM pg_class c
    JOIN pg_namespace ns ON ns.oid = c.relnamespace
    JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
   WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
     AND ns.nspname NOT IN ('audit', 'pg_catalog', 'information_schema');
  LOOP
    pass := pass + 1;
    nxt := '{}';
    FOREACH tbl IN ARRAY todo LOOP
      BEGIN
        EXECUTE format('DELETE FROM %s WHERE tenant_id = $1', tbl) USING tenant;
        GET DIAGNOSTICS n = ROW_COUNT;
        IF n > 0 THEN
          counts := counts || jsonb_build_object(tbl, coalesce((counts ->> tbl)::bigint, 0) + n);
        END IF;
      EXCEPTION WHEN foreign_key_violation THEN
        nxt := nxt || tbl;
      END;
    END LOOP;
    EXIT WHEN cardinality(nxt) = 0;
    IF pass >= 20 THEN
      RAISE EXCEPTION 'erase_tenant: rows of % stay referenced after % passes', nxt, pass;
    END IF;
    todo := nxt;
  END LOOP;
  RETURN counts;
END $$;
ALTER FUNCTION platform.erase_tenant(uuid) OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION platform.erase_tenant(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.erase_tenant(uuid) TO erp_worker;
