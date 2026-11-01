-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS payables.supplier_invoice_item_by_order;
