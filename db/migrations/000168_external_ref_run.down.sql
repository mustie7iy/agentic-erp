-- Dropping the column drops its foreign key with it.
ALTER TABLE integration.external_ref DROP COLUMN migration_run_id;
