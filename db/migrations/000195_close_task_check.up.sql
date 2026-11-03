-- A close task's check result (T64c; ADR-0058 §5, §6; 03-data-model
-- §3.22): :check runs a check task's check and records when it ran, who ran
-- it, whether it passed and what it found (the parked entries or the
-- ledgers, with their ids and names), and sets the task done when it
-- passed and open when it did not. A check task is done only while its
-- latest check passed: a person never completes one. A result is replaced
-- by the next check and kept when the task is reopened, never removed.
-- Expand only (06 §6.4): nullable columns without a default rewrite
-- nothing, and the checks are added NOT VALID, reading none of the rows
-- under the lock; 000196 validates them. The previous version writes no
-- check task (config.CloseChecks was empty), so its writes pass them.
ALTER TABLE closing.close_task
  ADD COLUMN checked_at    timestamptz,
  ADD COLUMN checked_by    uuid,           -- identity context: the person who ran the check
  ADD COLUMN check_passed  boolean,
  ADD COLUMN check_result  jsonb,          -- what the check found, as the API shows it
  -- A result is recorded whole, on a check task only.
  ADD CONSTRAINT close_task_checked CHECK (num_nulls(checked_at, checked_by, check_passed, check_result) IN (0, 4)
    AND (checked_at IS NULL OR kind = 'check') AND (check_result IS NULL OR jsonb_typeof(check_result) = 'object')) NOT VALID,
  -- A check task is done by its check passing.
  ADD CONSTRAINT close_task_check_passed CHECK (kind <> 'check' OR status <> 'done' OR check_passed IS TRUE) NOT VALID;

-- As 000194's, with the check result among what changes, and kept once
-- recorded.
CREATE OR REPLACE FUNCTION closing.check_close_task() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['assignee_id', 'status', 'completed_by', 'completed_at', 'note', 'version', 'updated_at', 'updated_by',
    'checked_at', 'checked_by', 'check_passed', 'check_result'];
BEGIN
  IF TG_OP = 'INSERT' THEN
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
