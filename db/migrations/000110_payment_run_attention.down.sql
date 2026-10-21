-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS payments.payment_run_attention;
