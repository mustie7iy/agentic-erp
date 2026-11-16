REVOKE UPDATE (parts) ON records.archiving_run FROM erp_app;
ALTER TABLE records.archiving_run DROP COLUMN parts;
