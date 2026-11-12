-- Assessment cycles and secondary cost elements (T60g; ADR-0064 §4-§5,
-- plan-breadth-week1 §T60g). An assessment charges the sender's primary
-- costs of a period to the receivers in one amount on the cycle's
-- assessment account, a secondary cost element: an account of the chart
-- that takes lines only from the controlling context's entries, each of
-- which balances on it, so it nets to zero in the company. Expand only
-- (06 §6.4): wider checks, a nullable column and rules about a value the
-- previous version never writes.

-- An account's cost element may be secondary, on an expense account alone
-- (a profit and loss account whose lines carry a cost center). The wider
-- check replaces the narrower one; every row satisfied the narrower, and
-- the table holds the charts' accounts alone, so it is validated at once.
ALTER TABLE ledger.gl_account DROP CONSTRAINT gl_account_cost_element;
ALTER TABLE ledger.gl_account ADD CONSTRAINT gl_account_cost_element CHECK (cost_element = 'none'
  OR (cost_element IN ('primary_cost', 'secondary') AND account_type = 'expense')
  OR (cost_element = 'revenue' AND account_type = 'revenue'));

-- An account is a secondary cost element from its creation and stays one:
-- an account that existed before may hold lines other contexts posted,
-- and a secondary one turned into another kind would pass its assessments
-- off as primary costs.
CREATE FUNCTION ledger.check_secondary_account() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.cost_element = 'secondary') <> (OLD.cost_element = 'secondary') THEN
    RAISE EXCEPTION 'account % changes its cost element from % to %: a secondary cost element is one from its creation and stays one',
      OLD.number, OLD.cost_element, NEW.cost_element USING ERRCODE = 'check_violation', CONSTRAINT = 'gl_account_secondary_kept';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_secondary_account() OWNER TO erp_owner;
CREATE TRIGGER gl_account_secondary_kept BEFORE UPDATE OF cost_element ON ledger.gl_account
  FOR EACH ROW EXECUTE FUNCTION ledger.check_secondary_account();

-- A line on a secondary cost element posts only in an entry of the
-- controlling context, whatever path wrote the line: manual and parked
-- entries, templates, a migration run's balances and every other context
-- are refused. The function is 000190's with the rule added.
CREATE OR REPLACE FUNCTION ledger.check_line() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE e ledger.journal_entry%ROWTYPE; kind text; element text; blocked boolean;
BEGIN
  SELECT * INTO e FROM ledger.journal_entry WHERE tenant_id = NEW.tenant_id AND fiscal_year = NEW.fiscal_year AND id = NEW.journal_entry_id;
  IF NEW.legal_entity_id <> e.legal_entity_id OR NEW.ledger_id <> e.ledger_id OR NEW.fiscal_period <> e.fiscal_period OR NEW.posting_date <> e.posting_date THEN
    RAISE EXCEPTION 'line % does not match its journal entry header', NEW.line_no USING ERRCODE = 'check_violation';
  END IF;
  SELECT a.reconciliation_kind, a.cost_element, ca.is_posting_blocked OR a.status <> 'active' INTO kind, element, blocked
  FROM ledger.gl_account a JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id
  WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.account_id AND ca.legal_entity_id = NEW.legal_entity_id;
  IF blocked THEN
    RAISE EXCEPTION 'account is blocked for posting' USING ERRCODE = 'check_violation';
  END IF;
  IF kind IN ('supplier', 'customer') AND NEW.partner_id IS NULL THEN
    RAISE EXCEPTION 'line % posts to a % reconciliation account without a partner', NEW.line_no, kind USING ERRCODE = 'check_violation';
  END IF;
  IF kind = 'asset' AND e.source_context NOT IN ('assets', 'integration') THEN
    RAISE EXCEPTION 'line % of entry % posts to an asset reconciliation account from %, which only assets post to', NEW.line_no, NEW.journal_entry_id,
      e.source_context USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_asset_reconciliation';
  END IF;
  IF element = 'secondary' AND e.source_context <> 'controlling' THEN
    RAISE EXCEPTION 'line % of entry % posts to a secondary cost element from %, which only controlling posts to', NEW.line_no, NEW.journal_entry_id,
      e.source_context USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_secondary_account';
  END IF;
  RETURN NEW;
END $$;

-- A cycle's kind may be assessment, and an assessment cycle names its
-- assessment account (the ledger's, no foreign key across schemas; the
-- controlling context checks it through ledger.AssignmentBalances), a
-- distribution none. The kind's check is replaced by the wider one; the
-- table is small and every row satisfies it.
ALTER TABLE controlling.allocation_cycle DROP CONSTRAINT allocation_cycle_kind_check;
ALTER TABLE controlling.allocation_cycle ADD CONSTRAINT allocation_cycle_kind_check CHECK (kind IN ('distribution', 'assessment'));
ALTER TABLE controlling.allocation_cycle ADD COLUMN assessment_account_id uuid,     -- ledger context
  ADD CONSTRAINT allocation_cycle_assessment_account CHECK ((kind = 'assessment') = (assessment_account_id IS NOT NULL));

-- A cycle keeps its assessment account too (000276's function with it).
CREATE OR REPLACE FUNCTION controlling.check_allocation_cycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.code, NEW.kind, NEW.sender_cost_center_id, NEW.assessment_account_id)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.code, OLD.kind, OLD.sender_cost_center_id, OLD.assessment_account_id) THEN
    RAISE EXCEPTION 'allocation cycle % keeps its company, code, kind, sender and assessment account', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_cycle_identity_kept';
  END IF;
  RETURN NEW;
END $$;
