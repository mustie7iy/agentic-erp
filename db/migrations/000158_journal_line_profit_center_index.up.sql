-- migrate:partitioned-index
-- A profit center's lines by company and period (T56d, ADR-0050 §5), as
-- 000157 for cost centers.
CREATE INDEX journal_line_profit_center ON ledger.journal_line (tenant_id, legal_entity_id, profit_center_id, fiscal_year, fiscal_period)
  WHERE profit_center_id IS NOT NULL;
