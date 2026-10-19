-- migrate:no-transaction
-- The business gauges (T54b) count open periods past their end date across
-- tenants. Partial, so the gauge reads only the open periods (a few per
-- ledger) and finds their dates by the fiscal_period key, rather than
-- scanning every period of every year. Built concurrently (06 §6.4); a
-- failed build leaves an invalid index, dropped by hand before the retry.
CREATE INDEX CONCURRENTLY period_status_open ON ledger.period_status (tenant_id, calendar_id, fiscal_year, period) WHERE status = 'open';
