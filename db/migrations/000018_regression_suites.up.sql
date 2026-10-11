-- Regression suites (T10): stratified samples of real supplier invoices with
-- the decision each got under a configuration version. The simulation of a
-- change request replays the latest suite of its base version and records
-- the result as regression evidence.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('config.regression.manage', 'Capture regression suites', false);

CREATE TABLE config.regression_suite (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  number          text NOT NULL,
  name            text NOT NULL CHECK (length(trim(name)) > 0),
  config_version  integer NOT NULL,
  window_days     smallint NOT NULL CHECK (window_days > 0),
  case_count      integer NOT NULL CHECK (case_count >= 0),
  strata          jsonb NOT NULL DEFAULT '[]',
  captured_at     timestamptz NOT NULL DEFAULT now(),
  created_at      timestamptz NOT NULL DEFAULT now(),
  created_by      uuid NOT NULL,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  updated_by      uuid NOT NULL,
  version         integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, number),
  FOREIGN KEY (tenant_id, config_version) REFERENCES config.config_version (tenant_id, number)
);
CREATE INDEX regression_suite_by_version ON config.regression_suite (tenant_id, config_version, captured_at);
CREATE TRIGGER regression_suite_touch BEFORE UPDATE ON config.regression_suite FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE config.regression_case (
  tenant_id            uuid NOT NULL,
  suite_id             uuid NOT NULL,
  case_no              integer NOT NULL CHECK (case_no > 0),
  supplier_invoice_id  uuid NOT NULL,
  document_number      text NOT NULL,
  legal_entity_id      uuid NOT NULL,
  company              text NOT NULL,
  why                  text NOT NULL,
  expected_decision    text NOT NULL,
  expected_issues      jsonb NOT NULL DEFAULT '[]',
  PRIMARY KEY (tenant_id, suite_id, case_no),
  FOREIGN KEY (tenant_id, suite_id) REFERENCES config.regression_suite (tenant_id, id)
);
-- A suite's cases are its record; they are never rewritten.
CREATE TRIGGER regression_case_immutable BEFORE UPDATE OR DELETE ON config.regression_case
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

SELECT platform.secure_table('config.regression_suite'::regclass);
SELECT platform.secure_table('config.regression_case'::regclass);
