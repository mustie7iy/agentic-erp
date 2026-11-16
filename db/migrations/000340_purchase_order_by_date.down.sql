-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS procurement.purchase_order_by_date;
