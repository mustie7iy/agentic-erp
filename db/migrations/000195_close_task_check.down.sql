CREATE OR REPLACE FUNCTION closing.check_close_task() RETURNS trigger
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
ALTER TABLE closing.close_task DROP CONSTRAINT close_task_check_passed, DROP CONSTRAINT close_task_checked,
  DROP COLUMN check_result, DROP COLUMN check_passed, DROP COLUMN checked_by, DROP COLUMN checked_at;
