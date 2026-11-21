-- migrate:no-transaction
-- review:batch86 (ADR-0061 §4, §15): the list's fetch_operation_id filter
-- matches the fetch that processed a document too, so a fetch's refused
-- count leads to the documents it refused, also those an earlier fetch
-- recorded. Built concurrently (06 §6.4); a failed build leaves an invalid
-- index, dropped by hand before the retry. Partial: a document is
-- processed once.
CREATE INDEX CONCURRENTLY einvoice_by_processed_fetch ON einvoicing.einvoice (tenant_id, processed_fetch_operation_id)
  WHERE processed_fetch_operation_id IS NOT NULL;
