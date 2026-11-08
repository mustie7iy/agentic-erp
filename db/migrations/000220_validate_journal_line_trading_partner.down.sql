-- A validated check cannot be marked unvalidated again: the down restores
-- 000219's state by adding it back NOT VALID.
ALTER TABLE ledger.journal_line DROP CONSTRAINT journal_line_trading_partner;
ALTER TABLE ledger.journal_line ADD CONSTRAINT journal_line_trading_partner
  CHECK (trading_partner_id <> legal_entity_id) NOT VALID;
