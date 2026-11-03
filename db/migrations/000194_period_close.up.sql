-- The period close and its manual checklist (T64b; ADR-0058 §5, §6;
-- 03-data-model §3.22): one close per company and regular period, covering
-- the ledgers it names, each with its posting period, and its tasks copied
-- from the configuration's close template when it is opened, with the
-- configuration version, so a later change of the template leaves it as it
-- was. legal_entity_id is the tenancy context's, the ledgers and periods
-- the ledger context's, the principals the identity context's: no foreign
-- key leaves the schema; the closing context checks them through its
-- ports. Every write on a close or its tasks reads the close FOR UPDATE and
-- moves its version, so a task action with a stale If-Match is refused.
-- Expand only: a new schema and permission, which the previous version
-- neither reads nor writes.
CREATE SCHEMA closing;
GRANT USAGE ON SCHEMA closing TO erp_app, erp_readonly, erp_owner;
SELECT platform.create_event_tables('closing');
SELECT platform.secure_table('closing.outbox'::regclass);
SELECT platform.secure_table('closing.inbox'::regclass);
GRANT UPDATE (published_at) ON closing.outbox TO erp_app;
GRANT USAGE ON SCHEMA closing TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON closing.outbox TO erp_relay;

-- A company's close of one regular period. Special periods (13-16) are
-- the year-end close's, which T64 leaves out (ADR-0058 §1).
CREATE TABLE closing.period_close (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,           -- tenancy context
  fiscal_year      smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  fiscal_period    smallint NOT NULL,
  period_end       date NOT NULL,           -- the leading ledger's last day of the period
  config_version   integer NOT NULL CHECK (config_version > 0),
  status           text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'signed_off')),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT period_close_regular CHECK (fiscal_period BETWEEN 1 AND 12),
  CONSTRAINT period_close_once UNIQUE (tenant_id, legal_entity_id, fiscal_year, fiscal_period)
);
-- The list pages by period end; the calendar reads a company's year.
CREATE INDEX period_close_by_end ON closing.period_close (tenant_id, period_end, id);
CREATE TRIGGER period_close_touch BEFORE UPDATE ON closing.period_close FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- A close starts open and moves open -> signed_off -> open (signed off,
-- then reopened after an unlock: ADR-0058 §7), or keeps its status while
-- its tasks change (its version moves). What it closes never changes.
CREATE FUNCTION closing.check_period_close() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['status', 'version', 'updated_at', 'updated_by'];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'open' THEN
      RAISE EXCEPTION 'a period close starts open, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'period_close_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION 'period close % changes only its status', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'period_close_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION closing.check_period_close() OWNER TO erp_owner;
CREATE TRIGGER period_close_transition BEFORE INSERT OR UPDATE ON closing.period_close FOR EACH ROW EXECUTE FUNCTION closing.check_period_close();
SELECT platform.secure_table('closing.period_close'::regclass);

-- The ledgers a close covers, each with its posting period of the close
-- (ledger context). Written with the close, never changed.
CREATE TABLE closing.close_ledger (
  tenant_id   uuid NOT NULL,
  close_id    uuid NOT NULL,
  ledger_id   uuid NOT NULL,                -- ledger context
  period_id   uuid NOT NULL,                -- ledger.period_status
  PRIMARY KEY (tenant_id, close_id, ledger_id),
  FOREIGN KEY (tenant_id, close_id) REFERENCES closing.period_close (tenant_id, id)
);
CREATE TRIGGER close_ledger_immutable BEFORE UPDATE OR DELETE ON closing.close_ledger FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('closing.close_ledger'::regclass);

-- A task of a close, copied from the template: a manual task a person
-- completes, or a check the system runs (T64c), due on a business day
-- counted from the period's end, waiting for the tasks it depends on.
CREATE TABLE closing.close_task (
  tenant_id     uuid NOT NULL,
  close_id      uuid NOT NULL,
  code          text NOT NULL CHECK (code ~ '^[A-Z0-9_]{1,30}$'),
  seq           smallint NOT NULL CHECK (seq BETWEEN 1 AND 200),
  name          text NOT NULL CHECK (btrim(name) <> '' AND char_length(name) <= 200),
  kind          text NOT NULL CHECK (kind IN ('manual', 'check')),
  check_kind    text,
  due_on        date NOT NULL,
  depends_on    text[] NOT NULL DEFAULT '{}',
  assignee_id   uuid,                      -- identity context: a person
  status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'done')),
  completed_by  uuid,
  completed_at  timestamptz,
  note          text CHECK (char_length(note) BETWEEN 1 AND 2000),
  version       integer NOT NULL DEFAULT 1,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  PRIMARY KEY (tenant_id, close_id, code),
  CONSTRAINT close_task_seq_once UNIQUE (tenant_id, close_id, seq),
  FOREIGN KEY (tenant_id, close_id) REFERENCES closing.period_close (tenant_id, id),
  -- A check task names its check; a manual task none.
  CONSTRAINT close_task_check_kind CHECK ((kind = 'check') = (check_kind IS NOT NULL)),
  -- Who completed it and when are set exactly while it is done, and its
  -- note only then.
  CONSTRAINT close_task_completed CHECK ((status = 'done') = (completed_by IS NOT NULL) AND (status = 'done') = (completed_at IS NOT NULL)
    AND (note IS NULL OR status = 'done')),
  -- It depends on other tasks, never on itself.
  CONSTRAINT close_task_depends_on CHECK (NOT code = ANY (depends_on) AND cardinality(depends_on) <= 200)
);
-- The worklist (GET /v1/close-tasks) pages in due date, close and code
-- order, a person's own by assignee.
CREATE INDEX close_task_by_due ON closing.close_task (tenant_id, due_on, close_id, code);
CREATE INDEX close_task_by_assignee ON closing.close_task (tenant_id, assignee_id, due_on) WHERE assignee_id IS NOT NULL;
CREATE TRIGGER close_task_touch BEFORE UPDATE ON closing.close_task FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- A task starts open and moves open <-> done, or keeps its status and
-- completion while it is assigned. What the template gave it never changes.
CREATE FUNCTION closing.check_close_task() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['assignee_id', 'status', 'completed_by', 'completed_at', 'note', 'version', 'updated_at', 'updated_by'];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'open' THEN
      RAISE EXCEPTION 'a close task starts open, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION 'close task % of close % changes only its assignee and status', OLD.code, OLD.close_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_fixed';
  END IF;
  -- A completion is recorded by completing the task and cleared by
  -- reopening it, never rewritten while it stands.
  IF NEW.status = OLD.status AND (NEW.completed_by, NEW.completed_at, NEW.note) IS DISTINCT FROM (OLD.completed_by, OLD.completed_at, OLD.note) THEN
    RAISE EXCEPTION 'close task % of close % keeps its completion while it is %', OLD.code, OLD.close_id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION closing.check_close_task() OWNER TO erp_owner;
CREATE TRIGGER close_task_transition BEFORE INSERT OR UPDATE ON closing.close_task FOR EACH ROW EXECUTE FUNCTION closing.check_close_task();
SELECT platform.secure_table('closing.close_task'::regclass);

-- Opening a close and working its checklist (ADR-0058 §13). Signing off
-- (T64d) has its own permission.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('closing.close.manage', 'Open period closes, and assign, complete and reopen their tasks', false);
