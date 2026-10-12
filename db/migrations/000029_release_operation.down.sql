REVOKE SELECT (resource) ON platform.operation FROM erp_worker;
REVOKE SELECT (release_operation_id) ON payments.payment_run FROM erp_worker;
ALTER TABLE payments.payment_run DROP COLUMN release_operation_id;
ALTER TABLE payments.payment_run DROP CONSTRAINT payment_run_status_check;
ALTER TABLE payments.payment_run ADD CONSTRAINT payment_run_status_check CHECK (status IN ('proposing', 'proposed', 'released', 'rejected', 'failed'));
