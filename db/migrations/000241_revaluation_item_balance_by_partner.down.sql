-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS ledger.revaluation_item_balance_by_partner;
