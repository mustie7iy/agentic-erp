-- Asset reconciliation accounts (T57a, ADR-0057 §3): a line on an account
-- whose reconciliation_kind is 'asset' posts only in an entry of the
-- assets context, or of a migration run (integration) loading balances, so
-- the account's balance is the assets' acquisition cost or accumulated
-- depreciation. Manual and parked entries, templates and every other
-- context are refused here, whatever path wrote the line. The function is
-- 000005's with the asset rule added; a reversal keeps its original's
-- source context, so an assets entry's reversal passes.
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

-- Reading asset classes and the assets context's reads (ADR-0057 §10).
-- Expand only: a new permission, which no role holds until it is granted.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('assets.asset.read', 'View asset classes and assets', false);
