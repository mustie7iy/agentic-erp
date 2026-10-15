-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS payments.bank_file_once_per_run;
