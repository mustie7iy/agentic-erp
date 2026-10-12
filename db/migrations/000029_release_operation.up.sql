-- Payment release as an operation (T27, ADR-0012): a release moves the run to
-- releasing and posts one payment per activity; the run is released when the
-- bank file is written. The dispatcher finds release operations across
-- tenants as the worker role.
ALTER TABLE payments.payment_run DROP CONSTRAINT payment_run_status_check;
ALTER TABLE payments.payment_run ADD CONSTRAINT payment_run_status_check CHECK (status IN ('proposing', 'proposed', 'releasing', 'released', 'rejected', 'failed'));
ALTER TABLE payments.payment_run ADD COLUMN release_operation_id uuid;  -- the latest release operation; its status says whether the release is under way or failed
GRANT SELECT (release_operation_id) ON payments.payment_run TO erp_worker;
GRANT SELECT (resource) ON platform.operation TO erp_worker;
