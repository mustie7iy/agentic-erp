REVOKE UPDATE (skip_reason) ON records.deletion_run_subject FROM erp_app;
ALTER TABLE records.deletion_run_subject DROP COLUMN skip_reason;
