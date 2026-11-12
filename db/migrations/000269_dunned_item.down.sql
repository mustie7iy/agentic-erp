DROP TABLE collections.dunned_item;
REVOKE UPDATE (status, version, updated_by) ON collections.dunning_run FROM erp_app;
DROP TRIGGER dunning_run_confirmed_once ON collections.dunning_run;
DROP FUNCTION collections.check_dunning_run_change();
-- The status check restored refuses the revert while a confirmed run
-- exists: the previous version knows proposals alone.
ALTER TABLE collections.dunning_run
  DROP CONSTRAINT dunning_run_status_check,
  ADD CONSTRAINT dunning_run_status_check CHECK (status IN ('proposed'));
