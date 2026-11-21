-- migrate:no-transaction
-- T65d (ADR-0061 §4, §15): the list's fetch_operation_id filter matches
-- the fetch that entered or held a document too, so a fetch's result
-- leads to the documents it entered and held, also those another fetch
-- recorded. Built concurrently (06 §6.4); a failed build leaves an
-- invalid index, dropped by hand before the retry. Partial: a document is
-- entered or held once.
CREATE INDEX CONCURRENTLY einvoice_by_entry_fetch ON einvoicing.einvoice (tenant_id, entry_fetch_operation_id)
  WHERE entry_fetch_operation_id IS NOT NULL;
