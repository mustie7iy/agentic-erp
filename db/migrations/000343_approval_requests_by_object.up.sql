-- migrate:no-transaction
-- The requests about a document (review:batch75): the audit evidence
-- export reads the requests about its journal entries' source documents
-- (approvals.Evidence.About) and listApprovalRequests filters by
-- object_id; without this index both read every request of the tenant.
-- The object leads after the tenant, then the order both read in
-- (created_at, id). Built concurrently (06 §6.4), so no request waits on
-- the table while it builds; a failed build leaves an invalid index,
-- dropped by hand before the retry.
CREATE INDEX CONCURRENTLY approval_request_by_object ON workflow.approval_request (tenant_id, object_id, created_at, id);
