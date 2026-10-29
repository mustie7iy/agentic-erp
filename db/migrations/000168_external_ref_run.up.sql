-- The migration run that loaded each external reference (review:batch23,
-- ADR-0034 As built, later runs). A later run tops up the GR/IR positions
-- of an order an earlier run of its own fiscal year loaded; it told the
-- earlier run by the take-over entries' source document, which an order
-- loaded with no position open does not have. The reference the run
-- records for every record it loads now names the run, so its fiscal year
-- (migration_run.scope) can be read for any loaded order.
--
-- Expand only (06 §6.4): a nullable column without a default rewrites
-- nothing, and the foreign key is added NOT VALID, which reads none of the
-- rows; 000169 validates it. References recorded before keep NULL: their
-- run cannot be told, and nothing is backfilled, so such an order is left
-- as before (a warning, and a difference in the reconciliation).
ALTER TABLE integration.external_ref ADD COLUMN migration_run_id uuid;
ALTER TABLE integration.external_ref ADD CONSTRAINT external_ref_migration_run_fkey FOREIGN KEY (tenant_id, migration_run_id)
  REFERENCES integration.migration_run (tenant_id, id) NOT VALID;
