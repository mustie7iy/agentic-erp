-- migrate:no-transaction
-- One items payment per supplier and run (T46c, 000080): built
-- concurrently (06 §6.4) before 000086 drops the constraint it replaces.
CREATE UNIQUE INDEX CONCURRENTLY payment_items_once ON payments.payment (tenant_id, payment_run_id, supplier_id) WHERE kind = 'items';
