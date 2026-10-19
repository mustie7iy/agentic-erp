-- migrate:no-transaction
-- A release writes one bank file per house bank and payment method (T45c,
-- ADR-0038), so a retried release step or two overlapping workflows never
-- write a second file for the same payments. Nulls are not distinct: a run
-- proposed for one bank has one file with neither (its bank), as the
-- previous version writes it, so this index keeps bank_file_once_per_run's
-- guarantee for it once 000095 drops that index. Built concurrently (06
-- §6.4); no IF NOT EXISTS, so an invalid index left by a failed build is
-- dropped by hand before the retry rather than passed over.
CREATE UNIQUE INDEX CONCURRENTLY bank_file_once_per_bank ON payments.bank_file (tenant_id, payment_run_id, house_bank_id, payment_method) NULLS NOT DISTINCT;
