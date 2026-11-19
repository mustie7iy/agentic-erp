DELETE FROM identity.sod_rule WHERE permission_a = 'collections.dispute.resolve' OR permission_b = 'collections.dispute.resolve';
DELETE FROM identity.role_permission WHERE permission_code = 'collections.dispute.resolve';
DELETE FROM ref.permission WHERE code = 'collections.dispute.resolve';
-- The previous version posts no collections entry; the narrower check is
-- restored NOT VALID, as 000377's down restores its own.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling', 'revenue',
                            'leases', 'inventory')) NOT VALID;
-- 000395's functions, as they were.
CREATE OR REPLACE FUNCTION collections.check_dispute_case_items() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  c collections.dispute_case;
  case_id uuid;
  n integer;
  total numeric;
  total_lc numeric;
  first_line boolean;
  mismatched integer;
BEGIN
  IF TG_TABLE_NAME = 'dispute_case' THEN
    case_id := NEW.id;
  ELSE
    case_id := NEW.dispute_case_id;
  END IF;
  SELECT * INTO c FROM collections.dispute_case WHERE tenant_id = NEW.tenant_id AND id = case_id;
  SELECT count(*), sum(i.disputed_amount), sum(i.disputed_amount_lc),
         bool_or(i.item_no = 1 AND i.fiscal_year = c.fiscal_year AND i.journal_entry_id = c.journal_entry_id AND i.line_no = c.line_no),
         count(*) FILTER (WHERE i.open <> (c.status = 'open'))
    INTO n, total, total_lc, first_line, mismatched
    FROM collections.dispute_case_item i WHERE i.tenant_id = c.tenant_id AND i.dispute_case_id = c.id;
  IF n = 0 OR total <> c.disputed_amount OR total_lc <> c.disputed_amount_lc OR NOT first_line OR mismatched > 0 THEN
    RAISE EXCEPTION 'dispute case %: its items are its amounts, its first is its own line, and they are open while it is', c.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dispute_case_items';
  END IF;
  RETURN NULL;
END $$;
CREATE OR REPLACE FUNCTION collections.check_dispute_case_item_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR (to_jsonb(NEW) - 'open') <> (to_jsonb(OLD) - 'open') OR (NEW.open AND NOT OLD.open) THEN
    RAISE EXCEPTION 'dispute case item %/%: an item stays as its case named it, and is closed with its case', OLD.dispute_case_id, OLD.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dispute_case_item_fixed';
  END IF;
  RETURN NEW;
END $$;
REVOKE UPDATE ON collections.dispute_case_item FROM erp_app;
ALTER TABLE collections.dispute_case_item DROP COLUMN credit_memo_number, DROP COLUMN credit_memo_id, DROP COLUMN accepted_amount_lc,
  DROP COLUMN accepted_amount;
REVOKE UPDATE ON collections.dispute_case FROM erp_app;
GRANT UPDATE (assignee_id, note, status, close_reason, closed_at, closed_by, version, updated_by) ON collections.dispute_case TO erp_app;
ALTER TABLE collections.dispute_case DROP COLUMN write_off_entry_number, DROP COLUMN write_off_entry_id, DROP COLUMN write_off_entry_year,
  DROP COLUMN posting_date, DROP COLUMN accepted_amount_lc, DROP COLUMN accepted_amount, DROP COLUMN resolution;
