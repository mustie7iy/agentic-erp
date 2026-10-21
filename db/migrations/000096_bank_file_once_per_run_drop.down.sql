-- migrate:no-transaction
-- Fails while a run has several files, which the previous schema cannot hold.
CREATE UNIQUE INDEX CONCURRENTLY bank_file_once_per_run ON payments.bank_file (tenant_id, payment_run_id);
