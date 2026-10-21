REVOKE SELECT (tenant_id, calendar_id, fiscal_year, period, end_date, is_special) ON ledger.fiscal_period FROM erp_worker;
REVOKE SELECT (tenant_id, calendar_id, fiscal_year, period, status) ON ledger.period_status FROM erp_worker;
REVOKE SELECT (created_at) ON workflow.approval_request FROM erp_worker;
REVOKE SELECT (status, updated_at) ON payments.bank_file FROM erp_worker;
REVOKE SELECT (updated_at) ON payments.payment_run FROM erp_worker;
REVOKE SELECT (status, created_at) ON payables.supplier_invoice FROM erp_worker;
REVOKE USAGE ON SCHEMA payables, ledger FROM erp_worker;
