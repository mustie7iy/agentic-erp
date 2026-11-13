-- Governance, risk and compliance (T98a; ADR-0078; 02 §2.2, 03-data-model
-- §3.31): the grc context's schema with its outbox and inbox, the
-- tenant's control library and the results of testing its controls. The
-- library is the tenant's own: no control is shipped. owner_id and
-- tested_by are the identity context's: no foreign key leaves the schema
-- (02 §2.1). Expand only: a new schema and three permissions, which the
-- previous version neither reads nor writes (06 §6.4).
CREATE SCHEMA grc;
GRANT USAGE ON SCHEMA grc TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('grc');
SELECT platform.secure_table('grc.outbox'::regclass);
SELECT platform.secure_table('grc.inbox'::regclass);
GRANT UPDATE (published_at) ON grc.outbox TO erp_app;
GRANT USAGE ON SCHEMA grc TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON grc.outbox TO erp_relay;

-- A control (ADR-0078 §2): what it is to achieve, the process it belongs
-- to in the tenant's own words, the person who owns it, how often it is
-- performed and whether a person or the system performs it. The code is
-- the tenant's own, unique whatever its case, and never changes. A
-- control is active or retired; it is retired once and never deleted.
CREATE TABLE grc.control (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  code        text NOT NULL CONSTRAINT control_code_check CHECK (code ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$'),
  objective   text NOT NULL CONSTRAINT control_objective_check CHECK (length(btrim(objective)) > 0 AND length(objective) <= 1000),
  process     text NOT NULL CONSTRAINT control_process_check CHECK (length(process) BETWEEN 1 AND 100 AND process = btrim(process)),
  owner_id    uuid NOT NULL,       -- identity context: a person
  frequency   text NOT NULL CONSTRAINT control_frequency_check
                CHECK (frequency IN ('continuous', 'daily', 'weekly', 'monthly', 'quarterly', 'yearly', 'ad_hoc')),
  nature      text NOT NULL CONSTRAINT control_nature_check CHECK (nature IN ('manual', 'automated')),
  status      text NOT NULL DEFAULT 'active' CONSTRAINT control_status_check CHECK (status IN ('active', 'retired')),
  version     integer NOT NULL DEFAULT 1 CONSTRAINT control_version_check CHECK (version >= 1),
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  uuid NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  updated_by  uuid NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
CREATE TRIGGER control_touch BEFORE UPDATE ON grc.control FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- A code is one control's in the tenant, whatever its case.
CREATE UNIQUE INDEX control_code_once ON grc.control (tenant_id, lower(code));
-- The library: the tenant's controls by code, in the order its pages are
-- read.
CREATE INDEX control_by_code ON grc.control (tenant_id, code, id);
SELECT platform.secure_table('grc.control'::regclass);

-- A retired control is fixed: retiring is its last change.
CREATE FUNCTION grc.check_control_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'retired' THEN
    RAISE EXCEPTION 'control %: a retired control is not changed (control_retired_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'control_retired_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION grc.check_control_change() OWNER TO erp_owner;
CREATE TRIGGER control_retired_fixed BEFORE UPDATE ON grc.control
  FOR EACH ROW EXECUTE FUNCTION grc.check_control_change();
-- A change sets what a control says and its status: its code stays, and
-- no control is deleted.
REVOKE UPDATE ON grc.control FROM erp_app;
GRANT UPDATE (objective, process, owner_id, frequency, nature, status, version, updated_by) ON grc.control TO erp_app;

-- The result of testing a control once (ADR-0078 §3): the date of the
-- test, passed or failed, a note of the evidence seen and the person who
-- tested. Insert-only: a correction is another test. The control's last
-- test is the one with the latest tested_on, the latest id (a UUIDv7,
-- so the one recorded last) among several of one date, which the index
-- reads first.
CREATE TABLE grc.control_test (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  control_id  uuid NOT NULL,
  tested_on   date NOT NULL,
  result      text NOT NULL CONSTRAINT control_test_result_check CHECK (result IN ('passed', 'failed')),
  note        text NOT NULL CONSTRAINT control_test_note_check CHECK (length(btrim(note)) > 0 AND length(note) <= 2000),
  tested_by   uuid NOT NULL,       -- identity context: the person who recorded it
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT control_test_control_fkey FOREIGN KEY (tenant_id, control_id) REFERENCES grc.control (tenant_id, id)
);
CREATE INDEX control_test_latest ON grc.control_test (tenant_id, control_id, tested_on DESC, id DESC);
CREATE TRIGGER control_test_immutable BEFORE UPDATE OR DELETE ON grc.control_test
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('grc.control_test'::regclass);

-- Reading the control library, recording and changing controls, and
-- recording the result of testing one (ADR-0078 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('grc.control.read', 'View the control library and the results of testing its controls', false),
  ('grc.control.write', 'Record, change and retire controls', false),
  ('grc.control.test', 'Record the result of testing a control', false);
