-- migrate:partitioned-index
-- A cost center's lines by company and period (T56d, ADR-0050 §5): its
-- balances (T56f) and the in-use checks of its changes. Built per partition
-- and attached (06 §6.4), postings going on meanwhile.
CREATE INDEX journal_line_cost_center ON ledger.journal_line (tenant_id, legal_entity_id, cost_center_id, fiscal_year, fiscal_period)
  WHERE cost_center_id IS NOT NULL;
