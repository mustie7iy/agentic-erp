-- A payment run's release writes one bank file (T27, ADR-0012): a release
-- activity run again after a lost completion, or two overlapping workflows,
-- must not write a second file that pays the suppliers twice.
CREATE UNIQUE INDEX bank_file_once_per_run ON payments.bank_file (tenant_id, payment_run_id);
