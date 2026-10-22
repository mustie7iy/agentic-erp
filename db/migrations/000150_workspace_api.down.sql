DROP TRIGGER audit_event_immutable ON audit.audit_event;
CREATE TRIGGER audit_event_immutable BEFORE UPDATE OR DELETE ON audit.audit_event FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
DROP FUNCTION audit.event_immutable();
-- erase_tenant as migration 000023 made it.
CREATE OR REPLACE FUNCTION platform.erase_tenant(tenant uuid) RETURNS jsonb
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
ALTER TABLE audit.audit_event DROP COLUMN client_address;
DROP INDEX workflow.approval_request_pending;
DELETE FROM identity.sod_rule WHERE 'identity.self.read' IN (permission_a, permission_b);
DELETE FROM identity.role_permission WHERE permission_code = 'identity.self.read';
DELETE FROM ref.permission WHERE code = 'identity.self.read';
