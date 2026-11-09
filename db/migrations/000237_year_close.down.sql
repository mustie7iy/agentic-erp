-- Reverses 000237: drops the year close's tables and restores the trigger
-- functions as 000230 left them.
DROP TABLE closing.year_close_task, closing.year_close_ledger, closing.year_close;

CREATE OR REPLACE FUNCTION closing.check_period_close() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['status', 'version', 'updated_at', 'updated_by'];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'open' AND NOT platform.importing(NEW.tenant_id) THEN
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
CREATE OR REPLACE FUNCTION closing.check_close_task() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['assignee_id', 'status', 'completed_by', 'completed_at', 'note', 'version', 'updated_at', 'updated_by',
    'checked_at', 'checked_by', 'check_passed', 'check_result'];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF platform.importing(NEW.tenant_id) THEN
      RETURN NEW;
    END IF;
    IF NEW.status <> 'open' THEN
      RAISE EXCEPTION 'a close task starts open, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
    END IF;
    IF NEW.checked_at IS NOT NULL THEN
      RAISE EXCEPTION 'a close task starts unchecked'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION 'close task % of close % changes only its assignee, status and check result', OLD.code, OLD.close_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_fixed';
  END IF;
  -- A completion is recorded by completing the task and cleared by
  -- reopening it, never rewritten while it stands.
  IF NEW.status = OLD.status AND (NEW.completed_by, NEW.completed_at, NEW.note) IS DISTINCT FROM (OLD.completed_by, OLD.completed_at, OLD.note) THEN
    RAISE EXCEPTION 'close task % of close % keeps its completion while it is %', OLD.code, OLD.close_id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
  END IF;
  -- A check result is replaced by the next check, never removed.
  IF OLD.checked_at IS NOT NULL AND NEW.checked_at IS NULL THEN
    RAISE EXCEPTION 'close task % of close % keeps its check result', OLD.code, OLD.close_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
  END IF;
  RETURN NEW;
END $$;
