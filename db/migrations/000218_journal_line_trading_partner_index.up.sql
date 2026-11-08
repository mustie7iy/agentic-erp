-- migrate:partitioned-index
-- A trading partner's lines by company (T68b, ADR-0059 §4): the journal
-- entries listed by trading partner (listJournalEntries?trading_partner_id).
-- Built per partition and attached (06 §6.4), postings going on meanwhile.
CREATE INDEX journal_line_by_trading_partner ON ledger.journal_line (tenant_id, legal_entity_id, trading_partner_id, fiscal_year, fiscal_period)
  WHERE trading_partner_id IS NOT NULL;
