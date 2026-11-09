-- migrate:no-transaction
-- A run values a flagged account's balance once per currency and trading
-- partner (T68b2, 000234), none being one of them: nulls are not distinct,
-- so the account's lines without a trading partner are still one balance.
-- 000236 drops revaluation_item_balance, which this index replaces. Built
-- concurrently (06 §6.4); no IF NOT EXISTS, so an invalid index left by a
-- failed build is dropped by hand before the retry rather than passed over.
CREATE UNIQUE INDEX CONCURRENTLY revaluation_item_balance_by_partner ON ledger.revaluation_item (tenant_id, run_id, account_id, currency_tc, trading_partner_id)
  NULLS NOT DISTINCT WHERE journal_entry_id IS NULL;
