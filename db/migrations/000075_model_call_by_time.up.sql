-- migrate:no-transaction
-- A month's usage report (getAiUsage, T42f) reads that month's calls of
-- the tenant, of every principal: model_call_by_principal leads with the
-- principal and so cannot serve it. Built concurrently (06 §6.4); a failed
-- build leaves an invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY model_call_by_time ON agents.model_call (tenant_id, called_at);
