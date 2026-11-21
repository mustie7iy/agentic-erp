-- migrate:no-transaction
-- As 000425 built it.
CREATE INDEX CONCURRENTLY IF NOT EXISTS einvoice_to_enter ON einvoicing.einvoice (tenant_id, connector_id, received_at, id)
  WHERE status = 'received' AND processed_at IS NOT NULL AND document_kind = 'invoice';
