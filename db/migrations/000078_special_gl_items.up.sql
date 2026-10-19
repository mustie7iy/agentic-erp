-- Special G/L items (T46a, ADR-0037, 03-data-model §3.7). A supplier line
-- may carry a special G/L indicator (A: down payment). It is then on the
-- legal entity's down payment account, an alternative reconciliation
-- account of the account determination (SAP T074), and every line on that
-- account carries it; the payments context's payable items leave such
-- lines out, so down payments never net in a run.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment'));

-- The column is null on every existing line, so the check holds on them;
-- adding it scans the journal once, as 000053's line checks did.
ALTER TABLE ledger.journal_line ADD COLUMN special_gl char(1);
ALTER TABLE ledger.journal_line ADD CONSTRAINT journal_line_special_gl
  CHECK (special_gl IS NULL OR (special_gl = 'A' AND partner_id IS NOT NULL));
CREATE INDEX journal_line_special_gl ON ledger.journal_line (tenant_id, legal_entity_id, partner_id) WHERE special_gl IS NOT NULL;

-- A line is on the legal entity's down payment account exactly when it
-- carries indicator A.
CREATE FUNCTION ledger.check_special_gl_line() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE on_account boolean;
BEGIN
  SELECT EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = NEW.tenant_id AND d.legal_entity_id = NEW.legal_entity_id
                 AND d.key = 'down_payment' AND d.account_id = NEW.account_id) INTO on_account;
  IF on_account <> (NEW.special_gl IS NOT DISTINCT FROM 'A') THEN
    RAISE EXCEPTION 'line % of entry % is on the down payment account exactly when it is a down payment (special G/L A)', NEW.line_no, NEW.journal_entry_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_line_special_gl_account';
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER journal_line_special_gl AFTER INSERT ON ledger.journal_line
  FOR EACH ROW EXECUTE FUNCTION ledger.check_special_gl_line();

-- The down payment account, once determined, does not change: its lines'
-- reversals post to it, and the line rule above would refuse them on
-- another.
CREATE FUNCTION ledger.check_down_payment_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.key = 'down_payment' AND NEW.account_id IS DISTINCT FROM OLD.account_id THEN
    RAISE EXCEPTION 'the down payment account of legal entity % cannot change once determined', OLD.legal_entity_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'account_determination_down_payment_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER account_determination_down_payment_fixed BEFORE UPDATE OF account_id ON ledger.account_determination
  FOR EACH ROW EXECUTE FUNCTION ledger.check_down_payment_fixed();
