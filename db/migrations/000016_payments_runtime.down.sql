REVOKE SELECT (tenant_id, operation_id, status) ON payments.payment_run FROM erp_worker;
REVOKE SELECT (tenant_id, id, kind, status) ON platform.operation FROM erp_worker;
REVOKE USAGE ON SCHEMA platform, payments FROM erp_worker;
DROP INDEX payments.payment_run_operation;
REVOKE UPDATE (journal_entry_year, journal_entry_id) ON payments.payment FROM erp_app;
DROP TRIGGER payment_posted_once ON payments.payment;
DROP FUNCTION payments.check_payment_update();
ALTER TABLE payments.payment_item DROP CONSTRAINT payment_item_amount_check;
ALTER TABLE payments.payment_item ADD CONSTRAINT payment_item_amount_check CHECK (amount > 0);
