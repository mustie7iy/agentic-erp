-- migrate:no-transaction
-- revaluation_item_balance (000107) allowed one balance per account and
-- currency; a run now records one per trading partner too, which
-- revaluation_item_balance_by_partner (000241) keeps unique (T68b2).
DROP INDEX CONCURRENTLY IF EXISTS ledger.revaluation_item_balance;
