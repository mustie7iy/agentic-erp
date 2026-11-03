DELETE FROM identity.sod_rule WHERE permission_a = 'assets.asset.read' OR permission_b = 'assets.asset.read';
DELETE FROM identity.role_permission WHERE permission_code = 'assets.asset.read';
DELETE FROM ref.permission WHERE code = 'assets.asset.read';

-- 000005's check_line, without the asset rule.
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
  RETURN NEW;
END $$;
