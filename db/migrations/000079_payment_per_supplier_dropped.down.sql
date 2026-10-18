-- Request payments do not survive the downgrade: the constraint allows
-- one payment per supplier and run.
SET LOCAL row_security = off; -- the down migration changes every tenant's rows (06 §6.4)
DELETE FROM payments.payment WHERE kind = 'down_payment';
ALTER TABLE payments.payment ADD CONSTRAINT payment_tenant_id_payment_run_id_supplier_id_key UNIQUE (tenant_id, payment_run_id, supplier_id);
