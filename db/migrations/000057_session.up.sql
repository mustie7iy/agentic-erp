-- Server-side sessions for the idle timeout and absolute lifetime of
-- people's sessions (T37f, ADR-0031, 03-data-model §3.4). A session is one
-- authentication at the identity provider: sha256 of the issuer and sid, or
-- of the issuer, subject and auth_time. It is touched at most once a minute
-- per replica, and an ended session never resumes. Sessions are ephemeral:
-- not exported, imported or fingerprinted (offboarding, ADR-0021 as
-- amended). A worker ends, audited, the sessions idle longer than any
-- tenant allows, and deletes them a week after they end; one keyed by sid
-- alone, which a refreshed token could otherwise start again, is kept 90
-- days.
CREATE TABLE identity.session (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  principal_id  uuid NOT NULL,
  session_key   bytea NOT NULL CHECK (length(session_key) = 32),
  started_at    timestamptz NOT NULL,
  authenticated_at timestamptz,          -- the token's auth_time; NULL when it had none (keyed by sid alone)
  last_seen_at  timestamptz NOT NULL,
  ended_at      timestamptz,
  end_reason    text CHECK (end_reason IN ('expired', 'idle', 'revoked')),
  created_at    timestamptz NOT NULL DEFAULT now(),
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, principal_id) REFERENCES identity.principal (tenant_id, id),
  CONSTRAINT session_end_reason CHECK ((ended_at IS NULL) = (end_reason IS NULL))
);
CREATE UNIQUE INDEX session_principal_key ON identity.session (tenant_id, principal_id, session_key);
CREATE INDEX session_last_seen ON identity.session (last_seen_at);

-- Only the last sighting and the end change; an ended session never
-- resumes.
CREATE FUNCTION identity.check_session_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.ended_at IS NOT NULL THEN
    RAISE EXCEPTION 'session % has ended', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.id <> OLD.id OR NEW.session_key <> OLD.session_key OR NEW.principal_id <> OLD.principal_id OR NEW.started_at <> OLD.started_at
     OR NEW.authenticated_at IS DISTINCT FROM OLD.authenticated_at THEN
    RAISE EXCEPTION 'session %: only last_seen_at and its end change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.last_seen_at < OLD.last_seen_at THEN
    RAISE EXCEPTION 'session %: last_seen_at only moves on', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER session_update BEFORE UPDATE ON identity.session FOR EACH ROW EXECUTE FUNCTION identity.check_session_update();
SELECT platform.secure_table('identity.session'::regclass);

-- The sweeper finds idle sessions and deletes ended ones across tenants;
-- it ends them in each tenant's scope as the application role, audited.
GRANT USAGE ON SCHEMA identity TO erp_worker;
GRANT SELECT (tenant_id, id, last_seen_at, ended_at, authenticated_at), DELETE ON identity.session TO erp_worker;
