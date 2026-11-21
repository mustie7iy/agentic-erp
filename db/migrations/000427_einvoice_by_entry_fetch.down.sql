-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS einvoicing.einvoice_by_entry_fetch;
