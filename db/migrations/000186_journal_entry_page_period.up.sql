-- migrate:partitioned-index
-- A period's entries in the page order (T73j2): a trial balance's account
-- drilled down to its entries in one period (listJournalEntries'
-- account_id and fiscal_period). The page index (000055) serves a whole
-- year by date, but a period's entries lie among the year's, so its first
-- page read every earlier period's entries first; this one starts at the
-- period. An account on few entries is found by its lines instead
-- (journal_line_account, 000005). Built per partition and attached
-- (06 §6.4), postings going on meanwhile.
CREATE INDEX journal_entry_page_period ON ledger.journal_entry (tenant_id, legal_entity_id, fiscal_period, posting_date, id);
