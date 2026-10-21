-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS payments.statement_match_line;
