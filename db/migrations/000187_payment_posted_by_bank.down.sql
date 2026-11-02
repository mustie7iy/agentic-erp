-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS payments.payment_posted_by_bank;
