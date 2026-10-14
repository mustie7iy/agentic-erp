-- migrate:no-transaction
-- A payment run's release writes one bank file (T27, ADR-0012): a release
-- activity run again after a lost completion, or two overlapping workflows,
-- must not write a second file that pays the suppliers twice. Built
-- concurrently (06-engineering-process §6.4). A run holding two files from
-- before this index makes the build fail and leaves an invalid index: the
-- duplicate is resolved by hand and the index dropped before the retry,
-- which is why there is no IF NOT EXISTS to pass over an invalid index.
CREATE UNIQUE INDEX CONCURRENTLY bank_file_once_per_run ON payments.bank_file (tenant_id, payment_run_id);
