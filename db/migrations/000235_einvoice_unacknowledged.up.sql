-- migrate:no-transaction
-- The business gauge erp_einvoices_unacknowledged (T65b, ADR-0061 §4)
-- counts the e-invoices not yet acknowledged to their provider. Cell-wide,
-- like the other gauges' indexes (000109-000112): the gauge counts across
-- tenants, so a key led by tenant_id could not serve it. Partial, so it
-- holds only the rows the gauge watches, which an acknowledgement after
-- the commit takes out at once. Built concurrently (06 §6.4); a failed
-- build leaves an invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY einvoice_unacknowledged ON einvoicing.einvoice (received_at) WHERE acknowledged_at IS NULL;
