-- The API the web workspace needs (T73c, ADR-0052 G1-G3) and the client
-- addresses of the API's audit trail (PO, 2026-10-21; ADR-0011 amended).
-- Expand only: a seeded permission, an index, a nullable column, and the
-- erasure and immutability functions replaced to cover the column.

-- G2, G3: reading who you are (getCurrentPrincipal) and the names of the
-- principals on what you see (listPrincipalNames). Every principal holds
-- it, whatever its roles (authz.Permissions), so it is granted to no role
-- and no segregation-of-duties rule may name it.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('identity.self.read', 'See who you are and the names of principals (every principal holds it)', false);

-- G1: the inbox, decidable_by=me, reads the pending requests newest first
-- (04 §4.5: the sort column and id after the filter it serves); the step's
-- role and the caller's approvals are looked up by their keys.
CREATE INDEX approval_request_pending ON workflow.approval_request (tenant_id, created_at, id) WHERE status = 'pending';

-- Where a request came from: a person's address in the workspace as
-- erp-web saw it, otherwise the connection's as the router saw it
-- (Erp-Client-Address, trusted from the router's identity alone). Null for
-- work no request made. Personal data: it is kept outside the hash chain
-- (audit.Write hashes the entry without it), so that the erasure can blank
-- it and leave the chain and its anchors intact (ADR-0026).
ALTER TABLE audit.audit_event ADD COLUMN client_address inet;

-- Events stay insert-only, as platform.forbid_mutation keeps them, but
-- for the erasure of their tenant blanking their addresses:
-- platform.erase_tenant alone sets app.erasing, and nothing else of an
-- event may change with it. A delete while app.erasing names the tenant
-- passes, as forbid_mutation lets it (the erasure itself leaves the audit
-- schema; a tenant's trail is removed so only by an operator moving it).
CREATE FUNCTION audit.event_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF coalesce(current_setting('app.erasing', true), '') = OLD.tenant_id::text THEN
    IF TG_OP = 'DELETE' THEN
      RETURN OLD;
    END IF;
    IF NEW.client_address IS NULL AND (to_jsonb(NEW) - 'client_address') = (to_jsonb(OLD) - 'client_address') THEN
      RETURN NEW;
    END IF;
  END IF;
  RAISE EXCEPTION '%.% is insert-only; % is not allowed', TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'insufficient_privilege';
END $$;
DROP TRIGGER audit_event_immutable ON audit.audit_event;
CREATE TRIGGER audit_event_immutable BEFORE UPDATE OR DELETE ON audit.audit_event FOR EACH ROW EXECUTE FUNCTION audit.event_immutable();

-- erase_tenant as before (migration 000023), and then the tenant's audit
-- events lose their client addresses: the trail stays, the personal data
-- in it goes (ADR-0021, ADR-0026). The count is reported under
-- audit.audit_event.client_address.
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
  UPDATE audit.audit_event SET client_address = NULL WHERE tenant_id = tenant AND client_address IS NOT NULL;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n > 0 THEN
    counts := counts || jsonb_build_object('audit.audit_event.client_address', n);
  END IF;
  RETURN counts;
END $$;
