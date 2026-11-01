-- migrate:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS workflow.approval_request_by_requester;
