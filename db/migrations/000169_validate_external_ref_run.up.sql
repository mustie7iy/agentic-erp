-- Validates the foreign key 000168 added NOT VALID (06-engineering-process
-- §6.4: expand, then validate in a later migration). Validation takes only
-- a SHARE UPDATE EXCLUSIVE lock, so writes go on while the rows are
-- scanned; every row has met it since 000168 (NULL, or a run of the
-- reference's tenant).
ALTER TABLE integration.external_ref VALIDATE CONSTRAINT external_ref_migration_run_fkey;
