-- Validates the checks 000153 added NOT VALID (06 §6.4), scanning the
-- table without blocking writes.
ALTER TABLE payments.payment_run VALIDATE CONSTRAINT payment_run_release_approval_check;
ALTER TABLE payments.payment_run VALIDATE CONSTRAINT payment_run_release_request;
ALTER TABLE payments.payment_run VALIDATE CONSTRAINT payment_run_release_approved;
ALTER TABLE payments.payment_run VALIDATE CONSTRAINT payment_run_release_sod;
