-- migrate:no-transaction
-- The business gauges (T54b) count invoices on hold and those parked
-- for days.
-- Cell-wide, like the queue indexes (outbox_unpublished,
-- balance_projection_queue): the gauge counts across tenants, so a key led
-- by tenant_id could not serve it. Partial, so it holds only the rows the
-- gauge watches. Built concurrently (06 §6.4); a failed build leaves an
-- invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY supplier_invoice_waiting ON payables.supplier_invoice (status, created_at) WHERE status IN ('parked', 'on_hold');
