-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000168's state by adding it back NOT VALID.
ALTER TABLE integration.external_ref DROP CONSTRAINT external_ref_migration_run_fkey;
ALTER TABLE integration.external_ref ADD CONSTRAINT external_ref_migration_run_fkey FOREIGN KEY (tenant_id, migration_run_id)
  REFERENCES integration.migration_run (tenant_id, id) NOT VALID;
