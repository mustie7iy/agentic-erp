DROP TRIGGER run_opening ON consolidation.run;
DROP FUNCTION consolidation.check_run_opening();
ALTER TABLE consolidation.run DROP COLUMN opening_run_id;
