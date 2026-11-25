-- migrate:partitioned-index
-- A ledger's page of journal entries in the list's other page orders (T52,
-- 000470 gives the reason). Built per partition and attached (06 §6.4).
CREATE INDEX journal_entry_page_ledger_period ON ledger.journal_entry (tenant_id, legal_entity_id, ledger_id, fiscal_period, posting_date, id);
