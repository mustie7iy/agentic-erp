-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS tenancy.org_unit_code_once;
