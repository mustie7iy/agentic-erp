-- migrate:no-transaction
-- Fails while a run pays a supplier's items in more than one payment,
-- which the previous schema cannot hold: payments are not deleted to
-- make room for it.
CREATE UNIQUE INDEX CONCURRENTLY payment_items_once ON payments.payment (tenant_id, payment_run_id, supplier_id) WHERE kind = 'items';
