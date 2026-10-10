-- Roles are cluster-wide and may hold grants in other databases, so the role
-- itself is kept; only this database's grants are removed.
DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass AS tbl, n.nspname AS schema
           FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
           WHERE c.relname = 'outbox' AND c.relkind = 'r' LOOP
    EXECUTE format('REVOKE ALL ON %s FROM erp_relay', r.tbl);
    EXECUTE format('REVOKE USAGE ON SCHEMA %I FROM erp_relay', r.schema);
  END LOOP;
END $$;
ALTER TABLE platform.idempotency_key DROP COLUMN response_headers;
