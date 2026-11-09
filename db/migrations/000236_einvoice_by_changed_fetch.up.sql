-- migrate:no-transaction
-- T65b (ADR-0061 §4): the list's fetch_operation_id filter matches the
-- fetch that recorded a document or found it changed. Built concurrently
-- (06 §6.4), alone, as a migration without a transaction runs one
-- statement; a failed build leaves an invalid index, dropped by hand
-- before the retry.
-- Partial, as few documents ever change.
CREATE INDEX CONCURRENTLY einvoice_by_changed_fetch ON einvoicing.einvoice (tenant_id, changed_fetch_operation_id)
  WHERE changed_fetch_operation_id IS NOT NULL;
