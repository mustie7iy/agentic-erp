-- Agent certification (ADR-0039): per agent type and concrete model, the
-- highest autonomy level an evaluation run certified, loaded by an
-- operator from the run's report (erp-server certify load). Platform
-- reference data, like ref.model_price: evaluations are of the product,
-- not of a tenant.
CREATE TABLE ref.agent_certification (
  agent_type     text NOT NULL CHECK (agent_type ~ '^[a-z][a-z0-9_]{0,49}$'),
  model          text NOT NULL CHECK (model ~ '^[a-z0-9][a-z0-9.-]{0,99}$'),
  -- Level 0 needs no certification: an agent at 0 only reads.
  level          smallint NOT NULL CONSTRAINT agent_certification_level_check CHECK (level BETWEEN 1 AND 2),
  suite_version  integer NOT NULL CHECK (suite_version > 0),
  report_sha256  text NOT NULL CONSTRAINT agent_certification_report_check CHECK (report_sha256 ~ '^[0-9a-f]{64}$'),
  certified_by   text NOT NULL CHECK (length(certified_by) BETWEEN 1 AND 200),  -- the operator who loaded it
  certified_at   timestamptz NOT NULL DEFAULT now(),
  -- A report is loaded once; the latest load for a type and model is its
  -- certification, a later run's lower level included.
  PRIMARY KEY (agent_type, model, report_sha256)
);
CREATE INDEX agent_certification_latest ON ref.agent_certification (agent_type, model, certified_at DESC);
-- A certification, once loaded, stands: a new run is a new row. The rows
-- and who loaded them are the certifications' audit trail.
CREATE TRIGGER agent_certification_immutable BEFORE UPDATE OR DELETE ON ref.agent_certification FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
ALTER TABLE ref.agent_certification OWNER TO erp_owner;
GRANT SELECT ON ref.agent_certification TO erp_app, erp_readonly;

-- What an agent is, whose certification its autonomy needs (ADR-0039). An
-- open enumeration the application checks. The agents that exist have
-- none, which lists them as uncertified: they keep their level (000077)
-- but are raised by none until their administrators name a certified type.
ALTER TABLE identity.agent_profile ADD COLUMN agent_type text CONSTRAINT agent_profile_agent_type_check CHECK (agent_type ~ '^[a-z][a-z0-9_]{0,49}$');
