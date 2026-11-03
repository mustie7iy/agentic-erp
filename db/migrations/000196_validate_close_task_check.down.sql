-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000195's state by adding each one back NOT VALID.
ALTER TABLE closing.close_task
  DROP CONSTRAINT close_task_check_passed,
  ADD CONSTRAINT close_task_check_passed CHECK (kind <> 'check' OR status <> 'done' OR check_passed IS TRUE) NOT VALID,
  DROP CONSTRAINT close_task_checked,
  ADD CONSTRAINT close_task_checked CHECK (num_nulls(checked_at, checked_by, check_passed, check_result) IN (0, 4)
    AND (checked_at IS NULL OR kind = 'check') AND (check_result IS NULL OR jsonb_typeof(check_result) = 'object')) NOT VALID;
