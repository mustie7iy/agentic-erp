-- migrate:partitioned-index
-- An internal order's lines by company and period (T60b, ADR-0064 §2): the
-- order's actual costs (GET /v1/internal-order-balances). Built per
-- partition and attached (06 §6.4), postings going on meanwhile.
CREATE INDEX journal_line_by_internal_order ON ledger.journal_line (tenant_id, legal_entity_id, internal_order_id, fiscal_year, fiscal_period)
  WHERE internal_order_id IS NOT NULL;
