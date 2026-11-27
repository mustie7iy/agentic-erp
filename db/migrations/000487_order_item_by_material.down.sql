-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS procurement.purchase_order_item_by_material;
