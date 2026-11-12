-- Dropping the column drops its check on every partition.
ALTER TABLE ledger.journal_line DROP COLUMN internal_order_id;
