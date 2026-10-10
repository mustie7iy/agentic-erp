-- Runtime support for the request pipeline and the outbox relay (ADR-0014).

-- Idempotent replays return the original headers (ETag, Location) as well as
-- the status and body.
ALTER TABLE platform.idempotency_key ADD COLUMN response_headers jsonb NOT NULL DEFAULT '{}';

-- The outbox relay reads every tenant's events, so it is the one role that
-- bypasses row-level security. It may only read outboxes and mark rows
-- published; it cannot see or change any business table.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'erp_relay') THEN CREATE ROLE erp_relay NOLOGIN BYPASSRLS; END IF;
END $$;

DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.oid::regclass AS tbl, n.nspname AS schema
           FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
           WHERE c.relname = 'outbox' AND c.relkind = 'r' LOOP
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO erp_relay', r.schema);
    EXECUTE format('GRANT SELECT, UPDATE (published_at) ON %s TO erp_relay', r.tbl);
  END LOOP;
END $$;
