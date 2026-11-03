-- Validates the checks 000196 added NOT VALID (06 §6.4), scanning the
-- table without blocking writes.
ALTER TABLE closing.close_task VALIDATE CONSTRAINT close_task_checked;
ALTER TABLE closing.close_task VALIDATE CONSTRAINT close_task_check_passed;
