-- migrate:no-transaction
-- A run without a house bank has a file per bank and method; 000095's index
-- holds one file per run for runs with a bank, which is all the previous
-- version writes, so the per-run index goes.
DROP INDEX CONCURRENTLY IF EXISTS payments.bank_file_once_per_run;
