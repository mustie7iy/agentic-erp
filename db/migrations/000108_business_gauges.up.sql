-- Business gauges (T54b, ADR-0043): the cell's metrics read, on each
-- scrape, how much work waits in each context, aggregated across tenants.
-- The worker role reads them (it already reads the queues across tenants,
-- BYPASSRLS since 000016), and only the columns a count needs: no amount,
-- party or document id is visible to it.
GRANT USAGE ON SCHEMA payables, ledger TO erp_worker;
GRANT SELECT (status, created_at) ON payables.supplier_invoice TO erp_worker;
GRANT SELECT (updated_at) ON payments.payment_run TO erp_worker;
GRANT SELECT (status, updated_at) ON payments.bank_file TO erp_worker;
GRANT SELECT (created_at) ON workflow.approval_request TO erp_worker;
GRANT SELECT (tenant_id, calendar_id, fiscal_year, period, status) ON ledger.period_status TO erp_worker;
GRANT SELECT (tenant_id, calendar_id, fiscal_year, period, end_date, is_special) ON ledger.fiscal_period TO erp_worker;
