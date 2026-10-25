-- Account assignment on the journal line (T56d; ADR-0050 §5; 03-data-model
-- §3.7): the cost center, profit center and segment of each line, as SAP's
-- ACDOCA carries RCNTR, PRCTR and SEGMENT. Expand only (06 §6.4): nullable
-- columns without a default rewrite no partition, and the foreign keys are
-- added NOT VALID to each partition, which reads none of its rows; 000156
-- validates them partition by partition and adds them to the parent, and
-- 000157-000158 build the indexes. Lines posted before keep NULL; nothing
-- is backfilled.
ALTER TABLE ledger.journal_line ADD COLUMN cost_center_id uuid, ADD COLUMN profit_center_id uuid, ADD COLUMN segment_id uuid;

-- PostgreSQL 17 cannot add a NOT VALID foreign key to a partitioned table,
-- so each partition gets its own under the parent's name; 000156 adds the
-- parent's, which takes the partitions' over once they are validated.
-- The cost center names the line's company with it, so a line never
-- carries another company's cost center; the profit center is one
-- assigned to the line's company.
DO $$
DECLARE p regclass;
BEGIN
  FOR p IN SELECT inhrelid::regclass FROM pg_inherits WHERE inhparent = 'ledger.journal_line'::regclass ORDER BY 1 LOOP
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT journal_line_cost_center_fkey FOREIGN KEY (tenant_id, cost_center_id, legal_entity_id)
      REFERENCES ledger.cost_center (tenant_id, id, legal_entity_id) NOT VALID', p);
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT journal_line_profit_center_fkey FOREIGN KEY (tenant_id, profit_center_id, legal_entity_id)
      REFERENCES ledger.profit_center_company (tenant_id, profit_center_id, legal_entity_id) NOT VALID', p);
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT journal_line_segment_fkey FOREIGN KEY (tenant_id, segment_id)
      REFERENCES ledger.segment (tenant_id, id) NOT VALID', p);
  END LOOP;
END $$;

-- Which lines must carry what (ADR-0050 §5's table), checked at commit
-- beside the balance trigger, for postings in the fiscal years a company's
-- center accounting applies from (ledger.company_controlling):
--   primary_cost: a cost center; none: no cost center; revenue: a cost
--   center only of a category that allows revenue on the posting date;
--   with profit center accounting, every P&L line a profit center.
-- A segment is always its profit center's, and none without one. A
-- reversal copies its original's lines (ADR-0050 §5: a reversal undoes, it
-- does not charge new cost) and is not held to the rules of its posting
-- date. The ledger derives and validates the assignment before it writes a
-- line (ledger.assign), so the check fails only for a writer that skipped
-- it: during the rolling upgrade, an instance of the previous release
-- posting for a company whose center accounting is on, which fails closed
-- rather than write lines without their centers.
CREATE FUNCTION ledger.check_line_assignment() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_reversal boolean; v_segment uuid; v_element text; v_balance_sheet boolean;
  v_cost_from smallint; v_profit_from smallint; v_revenue boolean;
BEGIN
  SELECT e.reversal_of_id IS NOT NULL INTO v_reversal FROM ledger.journal_entry e
  WHERE e.tenant_id = NEW.tenant_id AND e.fiscal_year = NEW.fiscal_year AND e.id = NEW.journal_entry_id;
  IF v_reversal THEN
    RETURN NULL;
  END IF;
  IF NEW.profit_center_id IS NOT NULL THEN
    SELECT p.segment_id INTO v_segment FROM ledger.profit_center p WHERE p.tenant_id = NEW.tenant_id AND p.id = NEW.profit_center_id;
  END IF;
  IF NEW.segment_id IS DISTINCT FROM v_segment THEN
    RAISE EXCEPTION 'line % of entry %: the segment is not its profit center''s', NEW.line_no, NEW.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_segment_of_profit_center';
  END IF;
  SELECT k.cost_center_accounting_from, k.profit_center_accounting_from INTO v_cost_from, v_profit_from
  FROM ledger.company_controlling k WHERE k.tenant_id = NEW.tenant_id AND k.legal_entity_id = NEW.legal_entity_id;
  IF NOT FOUND THEN
    RETURN NULL;
  END IF;
  SELECT g.cost_element, g.is_balance_sheet INTO v_element, v_balance_sheet FROM ledger.gl_account g
  WHERE g.tenant_id = NEW.tenant_id AND g.id = NEW.account_id;
  IF v_cost_from <= NEW.fiscal_year THEN
    IF v_element = 'primary_cost' AND NEW.cost_center_id IS NULL THEN
      RAISE EXCEPTION 'line % of entry %: a primary cost line needs a cost center', NEW.line_no, NEW.journal_entry_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_cost_center_required';
    END IF;
    IF v_element = 'none' AND NEW.cost_center_id IS NOT NULL THEN
      RAISE EXCEPTION 'line % of entry %: the account takes no cost center', NEW.line_no, NEW.journal_entry_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_cost_center_not_allowed';
    END IF;
    IF v_element = 'revenue' AND NEW.cost_center_id IS NOT NULL THEN
      SELECT c.revenue_allowed INTO v_revenue FROM ledger.cost_center_period p
      JOIN ledger.cost_center_category c ON c.tenant_id = p.tenant_id AND c.code = p.category
      WHERE p.tenant_id = NEW.tenant_id AND p.cost_center_id = NEW.cost_center_id AND p.retired_by_change_id IS NULL
        AND p.valid_from <= NEW.posting_date AND (p.valid_to IS NULL OR p.valid_to > NEW.posting_date);
      IF NOT coalesce(v_revenue, false) THEN
        RAISE EXCEPTION 'line % of entry %: the cost center''s category takes no revenue', NEW.line_no, NEW.journal_entry_id
          USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_cost_center_revenue';
      END IF;
    END IF;
  END IF;
  IF v_profit_from <= NEW.fiscal_year AND NOT v_balance_sheet AND NEW.profit_center_id IS NULL THEN
    RAISE EXCEPTION 'line % of entry %: a P&L line needs a profit center', NEW.line_no, NEW.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_profit_center_required';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION ledger.check_line_assignment() OWNER TO erp_owner;
CREATE CONSTRAINT TRIGGER journal_line_assignment AFTER INSERT ON ledger.journal_line
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_line_assignment();
