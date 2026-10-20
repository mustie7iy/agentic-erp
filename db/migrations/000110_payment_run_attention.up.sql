-- migrate:no-transaction
-- The business gauges (T54b) count runs awaiting release and runs that
-- failed recently.
-- Cell-wide, like the queue indexes (outbox_unpublished,
-- balance_projection_queue): the gauge counts across tenants, so a key led
-- by tenant_id could not serve it. Partial, so it holds only the rows the
-- gauge watches. Built concurrently (06 §6.4); a failed build leaves an
-- invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY payment_run_attention ON payments.payment_run (status, updated_at) WHERE status IN ('proposed', 'failed');
