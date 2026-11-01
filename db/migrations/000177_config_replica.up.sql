-- The replicas of the cell that are running, with the configuration keys
-- each one's version understands (ADR-0038 rollout, 06 §6.4): activating
-- a document that sets a key some running replica does not know is
-- refused, since that replica would read the document without it (an older
-- :release has no check of companies[].release_dual_limit). A replica
-- records itself at start and then every 30 seconds (configuration.Replica,
-- as erp_worker) and deletes its row when it stops; one that stopped
-- without doing so (a crash) stops counting once its last sighting is 5
-- minutes old, measured on the database's clock alone, so no replica's
-- clock decides whether another counts.
--
-- Cell-wide, not a tenant's: every tenant's activation reads every row.
-- Row-level security is on, as on every table of the schema, with a policy
-- that lets the application read (only read) all rows; erp_worker, which
-- bypasses it, writes them.
CREATE TABLE config.replica (
  id           uuid PRIMARY KEY,
  version      text NOT NULL CHECK (length(version) BETWEEN 1 AND 100),  -- buildinfo: the release, or devel
  revision     text NOT NULL CHECK (length(revision) <= 100),            -- the git commit; empty when not recorded
  config_keys  text[] NOT NULL CHECK (cardinality(config_keys) > 0),     -- config.Keys() of the replica's binary
  started_at   timestamptz NOT NULL DEFAULT now(),
  seen_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX replica_seen ON config.replica (seen_at);
ALTER TABLE config.replica OWNER TO erp_owner;
ALTER TABLE config.replica ENABLE ROW LEVEL SECURITY;
ALTER TABLE config.replica FORCE ROW LEVEL SECURITY;
CREATE POLICY replica_read ON config.replica FOR SELECT TO erp_app USING (true);
GRANT SELECT ON config.replica TO erp_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON config.replica TO erp_worker;
