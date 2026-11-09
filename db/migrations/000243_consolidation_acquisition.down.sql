DROP TABLE consolidation.acquisition_equity;
DROP TABLE consolidation.acquisition;
DROP FUNCTION consolidation.check_acquisition();
DROP TABLE consolidation.run_acquisition;
ALTER TABLE consolidation.run_entry_line DROP COLUMN component;
