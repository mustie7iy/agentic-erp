-- migrate:no-transaction
-- Fails while a run holds two balances of one account and currency (two
-- trading partners): such a run is recorded only under 000236.
CREATE UNIQUE INDEX CONCURRENTLY revaluation_item_balance ON ledger.revaluation_item (tenant_id, run_id, account_id, currency_tc)
  WHERE journal_entry_id IS NULL;
