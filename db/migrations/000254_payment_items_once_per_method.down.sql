-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS payments.payment_items_once_per_method;
