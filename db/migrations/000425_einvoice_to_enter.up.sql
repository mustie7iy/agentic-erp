-- migrate:no-transaction
-- T65d (ADR-0061 §4, §6): a fetch enters its connector's processed tax
-- invoices still received, oldest first, beside those not yet processed
-- (einvoice_unprocessed); credit notes wait for T65e and are not in it.
-- Built concurrently (06 §6.4); a failed build leaves an invalid index,
-- dropped by hand before the retry.
CREATE INDEX CONCURRENTLY einvoice_to_enter ON einvoicing.einvoice (tenant_id, connector_id, received_at, id)
  WHERE status = 'received' AND processed_at IS NOT NULL AND document_kind = 'invoice';
