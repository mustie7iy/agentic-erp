-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS identity.principal_issuer_subject;
