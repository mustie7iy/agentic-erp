-- A supplier's down payment requests are paid by payments of their own
-- beside its items payment (T46c): UNIQUE (tenant_id, payment_run_id,
-- supplier_id) gives way to payment_items_once (000076), built already.
ALTER TABLE payments.payment DROP CONSTRAINT payment_tenant_id_payment_run_id_supplier_id_key;
