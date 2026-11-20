-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS tenancy.storage_location_code_once;
