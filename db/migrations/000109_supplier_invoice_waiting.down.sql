-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS payables.supplier_invoice_waiting;
