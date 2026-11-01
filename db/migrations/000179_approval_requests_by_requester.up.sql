-- listApprovalRequests' requested_by=me (T73d2, ADR-0052): the requests a
-- principal asked for, newest first (04 §4.5: the filter leads after the
-- tenant, then the sort column and id). Expand only: an index.
CREATE INDEX approval_request_by_requester ON workflow.approval_request (tenant_id, requester_id, created_at, id);
