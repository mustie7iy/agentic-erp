-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS einvoicing.einvoice_by_changed_fetch;
