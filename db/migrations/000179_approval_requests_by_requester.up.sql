-- migrate:no-transaction
-- listApprovalRequests' requested_by=me (T73d2, ADR-0052): the requests a
-- principal asked for, newest first (04 §4.5: the filter leads after the
-- tenant, then the sort column and id). Built concurrently (06 §6.4), so
-- no request waits on the table while it builds; a failed build leaves an
-- invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY approval_request_by_requester ON workflow.approval_request (tenant_id, requester_id, created_at, id);
