-- The year close and its manual checklist (T64m4; ADR-0060 §8; 03-data-model
-- §3.22): one close per company and fiscal year, covering the ledgers it
-- names, its tasks copied from the configuration's year tasks
-- (close.year_tasks) when it is opened, with the configuration version.
-- A year close is stored apart from period closes, with the same columns
-- and rules as their ADR-0058 §5 counterparts but for the period: a replica
-- of the release before reads every period_close row and would fail on one
-- without a period during a rolling deploy (ADR-0060 §8). The covered
-- ledgers carry no period: a year close covers each one's periods of the
-- year, which the closing context reads through the ledger's port. The
-- sign-off (year_sign_off) is T64m6's. legal_entity_id is the tenancy
-- context's, the ledgers the ledger context's, the principals the identity
-- context's: no foreign key leaves the schema. Every write on a close or
-- its tasks reads the close FOR UPDATE and moves its version.
-- The trigger functions of 000194 and 000196, as 000230 left them (an
-- import restores rows in any state), are shared: they now name the
-- table they guard (TG_TABLE_NAME) in their messages and constraint names,
-- which for the period close's tables are what they were.
-- Expand only: new tables, which the previous version neither reads nor
-- writes, and functions whose behaviour on its tables is unchanged.

-- A company's close of one fiscal year.
CREATE TABLE closing.year_close (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,           -- tenancy context
  fiscal_year      smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  year_end         date NOT NULL,           -- the leading ledger's last day of the year (its period 12's)
  config_version   integer NOT NULL CHECK (config_version > 0),
  status           text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'signed_off')),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT year_close_once UNIQUE (tenant_id, legal_entity_id, fiscal_year)
);
-- The list pages by year end.
CREATE INDEX year_close_by_end ON closing.year_close (tenant_id, year_end, id);
CREATE TRIGGER year_close_touch BEFORE UPDATE ON closing.year_close FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- As 000230's, naming the table it guards: a close starts open and moves
-- open -> signed_off -> open, or keeps its status while its tasks change.
CREATE OR REPLACE FUNCTION closing.check_period_close() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['status', 'version', 'updated_at', 'updated_by'];
  what text := replace(TG_TABLE_NAME, '_', ' ');
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'open' AND NOT platform.importing(NEW.tenant_id) THEN
      RAISE EXCEPTION 'a % starts open, not %', what, NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = TG_TABLE_NAME || '_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION '% % changes only its status', what, OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = TG_TABLE_NAME || '_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER year_close_transition BEFORE INSERT OR UPDATE ON closing.year_close FOR EACH ROW EXECUTE FUNCTION closing.check_period_close();
SELECT platform.secure_table('closing.year_close'::regclass);

-- The ledgers a year close covers (ledger context). Written with the
-- close, never changed.
CREATE TABLE closing.year_close_ledger (
  tenant_id   uuid NOT NULL,
  close_id    uuid NOT NULL,
  ledger_id   uuid NOT NULL,                -- ledger context
  PRIMARY KEY (tenant_id, close_id, ledger_id),
  FOREIGN KEY (tenant_id, close_id) REFERENCES closing.year_close (tenant_id, id)
);
CREATE TRIGGER year_close_ledger_immutable BEFORE UPDATE OR DELETE ON closing.year_close_ledger FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('closing.year_close_ledger'::regclass);

-- A task of a year close, copied from the year tasks: as closing.close_task
-- (000194, 000196), due on a business day counted from the year's end.
CREATE TABLE closing.year_close_task (
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
  checked_at    timestamptz,
  checked_by    uuid,                      -- identity context: the person who ran the check
  check_passed  boolean,
  check_result  jsonb,                     -- what the check found, as the API shows it
  version       integer NOT NULL DEFAULT 1,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  PRIMARY KEY (tenant_id, close_id, code),
  CONSTRAINT year_close_task_seq_once UNIQUE (tenant_id, close_id, seq),
  FOREIGN KEY (tenant_id, close_id) REFERENCES closing.year_close (tenant_id, id),
  -- A check task names its check; a manual task none.
  CONSTRAINT year_close_task_check_kind CHECK ((kind = 'check') = (check_kind IS NOT NULL)),
  -- Who completed it and when are set exactly while it is done, and its
  -- note only then.
  CONSTRAINT year_close_task_completed CHECK ((status = 'done') = (completed_by IS NOT NULL) AND (status = 'done') = (completed_at IS NOT NULL)
    AND (note IS NULL OR status = 'done')),
  -- It depends on other tasks, never on itself.
  CONSTRAINT year_close_task_depends_on CHECK (NOT code = ANY (depends_on) AND cardinality(depends_on) <= 200),
  -- A result is recorded whole, on a check task only.
  CONSTRAINT year_close_task_checked CHECK (num_nulls(checked_at, checked_by, check_passed, check_result) IN (0, 4)
    AND (checked_at IS NULL OR kind = 'check') AND (check_result IS NULL OR jsonb_typeof(check_result) = 'object')),
  -- A check task is done by its check passing.
  CONSTRAINT year_close_task_check_passed CHECK (kind <> 'check' OR status <> 'done' OR check_passed IS TRUE)
);
CREATE TRIGGER year_close_task_touch BEFORE UPDATE ON closing.year_close_task FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- As 000230's, naming the table it guards: a task starts open and
-- unchecked, moves open <-> done, keeps what the template gave it and its
-- completion while it stands, and a check result once recorded.
CREATE OR REPLACE FUNCTION closing.check_close_task() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['assignee_id', 'status', 'completed_by', 'completed_at', 'note', 'version', 'updated_at', 'updated_by',
    'checked_at', 'checked_by', 'check_passed', 'check_result'];
  what text := replace(TG_TABLE_NAME, '_', ' ');
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF platform.importing(NEW.tenant_id) THEN
      RETURN NEW;
    END IF;
    IF NEW.status <> 'open' THEN
      RAISE EXCEPTION 'a % starts open, not %', what, NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = TG_TABLE_NAME || '_transition';
    END IF;
    IF NEW.checked_at IS NOT NULL THEN
      RAISE EXCEPTION 'a % starts unchecked', what
        USING ERRCODE = 'check_violation', CONSTRAINT = TG_TABLE_NAME || '_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION '% % of close % changes only its assignee, status and check result', what, OLD.code, OLD.close_id
      USING ERRCODE = 'check_violation', CONSTRAINT = TG_TABLE_NAME || '_fixed';
  END IF;
  -- A completion is recorded by completing the task and cleared by
  -- reopening it, never rewritten while it stands.
  IF NEW.status = OLD.status AND (NEW.completed_by, NEW.completed_at, NEW.note) IS DISTINCT FROM (OLD.completed_by, OLD.completed_at, OLD.note) THEN
    RAISE EXCEPTION '% % of close % keeps its completion while it is %', what, OLD.code, OLD.close_id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = TG_TABLE_NAME || '_transition';
  END IF;
  -- A check result is replaced by the next check, never removed.
  IF OLD.checked_at IS NOT NULL AND NEW.checked_at IS NULL THEN
    RAISE EXCEPTION '% % of close % keeps its check result', what, OLD.code, OLD.close_id
      USING ERRCODE = 'check_violation', CONSTRAINT = TG_TABLE_NAME || '_transition';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER year_close_task_transition BEFORE INSERT OR UPDATE ON closing.year_close_task FOR EACH ROW EXECUTE FUNCTION closing.check_close_task();
SELECT platform.secure_table('closing.year_close_task'::regclass);
