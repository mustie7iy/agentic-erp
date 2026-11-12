-- Assessment cycles go with their runs, which a distribution-only version
-- cannot read; their entries stay in the journal. Secondary cost elements
-- become accounts without a cost object, as the narrower check allows.
DELETE FROM controlling.allocation_run WHERE (tenant_id, cycle_id) IN (SELECT tenant_id, id FROM controlling.allocation_cycle WHERE kind = 'assessment');
DELETE FROM controlling.allocation_receiver WHERE (tenant_id, cycle_id) IN (SELECT tenant_id, id FROM controlling.allocation_cycle WHERE kind = 'assessment');
DELETE FROM controlling.allocation_cycle WHERE kind = 'assessment';

-- 000276's function, without the assessment account.
CREATE OR REPLACE FUNCTION controlling.check_allocation_cycle() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.code, NEW.kind, NEW.sender_cost_center_id) IS DISTINCT FROM (OLD.legal_entity_id, OLD.code, OLD.kind, OLD.sender_cost_center_id) THEN
    RAISE EXCEPTION 'allocation cycle % keeps its company, code, kind and sender', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'allocation_cycle_identity_kept';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE controlling.allocation_cycle DROP CONSTRAINT allocation_cycle_assessment_account, DROP COLUMN assessment_account_id;
ALTER TABLE controlling.allocation_cycle DROP CONSTRAINT allocation_cycle_kind_check;
ALTER TABLE controlling.allocation_cycle ADD CONSTRAINT allocation_cycle_kind_check CHECK (kind IN ('distribution'));

-- 000190's check_line, without the secondary rule.
CREATE OR REPLACE FUNCTION ledger.check_line() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE e ledger.journal_entry%ROWTYPE; kind text; blocked boolean;
BEGIN
  SELECT * INTO e FROM ledger.journal_entry WHERE tenant_id = NEW.tenant_id AND fiscal_year = NEW.fiscal_year AND id = NEW.journal_entry_id;
  IF NEW.legal_entity_id <> e.legal_entity_id OR NEW.ledger_id <> e.ledger_id OR NEW.fiscal_period <> e.fiscal_period OR NEW.posting_date <> e.posting_date THEN
    RAISE EXCEPTION 'line % does not match its journal entry header', NEW.line_no USING ERRCODE = 'check_violation';
  END IF;
  SELECT a.reconciliation_kind, ca.is_posting_blocked OR a.status <> 'active' INTO kind, blocked
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
  RETURN NEW;
END $$;
DROP TRIGGER gl_account_secondary_kept ON ledger.gl_account;
DROP FUNCTION ledger.check_secondary_account();
UPDATE ledger.gl_account SET cost_element = 'none', version = version + 1 WHERE cost_element = 'secondary';
ALTER TABLE ledger.gl_account DROP CONSTRAINT gl_account_cost_element;
ALTER TABLE ledger.gl_account ADD CONSTRAINT gl_account_cost_element CHECK (cost_element = 'none' OR (cost_element = 'primary_cost' AND account_type = 'expense')
  OR (cost_element = 'revenue' AND account_type = 'revenue'));
