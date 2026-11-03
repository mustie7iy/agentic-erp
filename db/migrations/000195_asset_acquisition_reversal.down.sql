DROP TRIGGER document_entry_reversed_once ON assets.document_entry;
DROP FUNCTION assets.check_document_entry();
REVOKE UPDATE (reversal_entry_year, reversal_entry_id, reversal_document_number) ON assets.document_entry FROM erp_app;
ALTER TABLE assets.document_entry DROP CONSTRAINT document_entry_reversal_number, DROP COLUMN reversal_document_number;
CREATE OR REPLACE FUNCTION assets.check_clearing_use() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.currency, NEW.line_amount, NEW.line_lc, NEW.line_gc)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.currency, OLD.line_amount, OLD.line_lc, OLD.line_gc) THEN
    RAISE EXCEPTION 'line % of entry % keeps its amounts', OLD.line_no, OLD.entry_id USING ERRCODE = 'check_violation', CONSTRAINT = 'clearing_use_line_fixed';
  END IF;
  RETURN NEW;
END $$;
DROP INDEX assets.clearing_use_by_reversed;
ALTER TABLE assets.clearing_use DROP CONSTRAINT clearing_use_reverses, DROP COLUMN reverses_entry_id, DROP COLUMN reverses_entry_year;
CREATE OR REPLACE FUNCTION assets.check_acquisition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'posted' THEN
      RAISE EXCEPTION 'an acquisition starts posted, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'acquisition_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status <> OLD.status AND (OLD.status, NEW.status) <> ('posted', 'reversed') THEN
    RAISE EXCEPTION 'acquisition % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'acquisition_transition';
  END IF;
  IF (NEW.legal_entity_id, NEW.asset_id, NEW.seq, NEW.value_date, NEW.posting_date, NEW.fiscal_year, NEW.currency, NEW.amount, NEW.amount_lc,
      NEW.amount_gc, NEW.currency_lc, NEW.currency_gc, NEW.source_entry_year, NEW.source_entry_id, NEW.source_line_no, NEW.source_document_number,
      NEW.source_context, NEW.source_document_id, NEW.offset_account_id, NEW.text, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.asset_id, OLD.seq, OLD.value_date, OLD.posting_date, OLD.fiscal_year, OLD.currency, OLD.amount,
      OLD.amount_lc, OLD.amount_gc, OLD.currency_lc, OLD.currency_gc, OLD.source_entry_year, OLD.source_entry_id, OLD.source_line_no,
      OLD.source_document_number, OLD.source_context, OLD.source_document_id, OLD.offset_account_id, OLD.text, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'acquisition % keeps what it posted', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'acquisition_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE assets.acquisition DROP CONSTRAINT acquisition_reversal_date, DROP CONSTRAINT acquisition_reversal,
  DROP COLUMN reversal_reason, DROP COLUMN reversal_posting_date;
