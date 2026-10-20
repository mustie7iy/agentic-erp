-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS payments.house_bank_account_once;
