-- migrate:no-transaction
-- listBankStatementPayments (T73g2, ADR-0047): the posted payments that
-- name their house bank, latest first, the search's order (04 §4.5:
-- the filter leads after the tenant, then the sort column and id). A
-- payment that leaves its bank null is its run's (000093), read through
-- the run's payments. Built concurrently (06 §6.4), so no payment waits on
-- the table while it builds; a failed build leaves an invalid index,
-- dropped by hand before the retry.
CREATE INDEX CONCURRENTLY payment_posted_by_bank ON payments.payment (tenant_id, house_bank_id, created_at, id) WHERE journal_entry_id IS NOT NULL;
