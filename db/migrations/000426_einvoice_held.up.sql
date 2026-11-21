-- migrate:no-transaction
-- The business gauge erp_einvoices_held (T65d, ADR-0061 §4) counts the
-- e-invoices held for a person. Cell-wide, like the other gauges' indexes
-- (000109-000112, 000235): the gauge counts across tenants, so a key led by
-- tenant_id could not serve it. Partial, so it holds only the rows the
-- gauge watches. Built concurrently (06 §6.4); a failed build leaves an
-- invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY einvoice_held ON einvoicing.einvoice (received_at) WHERE status = 'held';
