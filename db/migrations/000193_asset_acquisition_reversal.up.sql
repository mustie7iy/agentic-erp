-- Manual acquisitions and the reversal of acquisitions (T57d; ADR-0057 §5,
-- §6; 03-data-model §3.21). An acquisition keeps its reversal's posting
-- date and reason; the journal entry of each assets document per ledger
-- records its reversal entry, the one update document_entry admits; and
-- what is used of a reversal's line names the line it reverses, so an
-- asset's take-off from it is bounded by what it holds from that line
-- without asking the ledger. Expand only: new nullable columns the
-- previous version neither reads nor writes.

ALTER TABLE assets.acquisition
  ADD COLUMN reversal_posting_date date,
  ADD COLUMN reversal_reason text CHECK (length(reversal_reason) BETWEEN 1 AND 2000),
  -- Reversed exactly when the reversal is recorded, never before the
  -- acquisition's posting date (assets.reversal_before_original).
  ADD CONSTRAINT acquisition_reversal CHECK ((status = 'reversed') = (reversal_posting_date IS NOT NULL)
    AND (reversal_posting_date IS NULL) = (reversal_reason IS NULL)),
  ADD CONSTRAINT acquisition_reversal_date CHECK (reversal_posting_date >= posting_date);

-- An acquisition starts posted and is reversed at most once; what it
-- posted never changes, nor its reversal once recorded.
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
      OLD.source_document_number, OLD.source_context, OLD.source_document_id, OLD.offset_account_id, OLD.text, OLD.created_at, OLD.created_by)
      OR (OLD.status = 'reversed' AND (NEW.reversal_posting_date, NEW.reversal_reason) IS DISTINCT FROM (OLD.reversal_posting_date, OLD.reversal_reason)) THEN
    RAISE EXCEPTION 'acquisition % keeps what it posted', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'acquisition_fixed';
  END IF;
  RETURN NEW;
END $$;

-- The line a reversal's line reverses: the same line number of the
-- reversed entry (the ledger's reversal keeps its original's lines in
-- order). Null for every other line.
ALTER TABLE assets.clearing_use
  ADD COLUMN reverses_entry_year smallint,
  ADD COLUMN reverses_entry_id uuid,
  ADD CONSTRAINT clearing_use_reverses CHECK ((reverses_entry_year IS NULL) = (reverses_entry_id IS NULL));
-- An asset's take-off from the reversal of a line it holds is read by the
-- line reversed.
CREATE INDEX clearing_use_by_reversed ON assets.clearing_use (tenant_id, reverses_entry_id, line_no) WHERE reverses_entry_id IS NOT NULL;

-- The line's amounts, and the line it reverses, are the posted line's,
-- which never change.
CREATE OR REPLACE FUNCTION assets.check_clearing_use() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.currency, NEW.line_amount, NEW.line_lc, NEW.line_gc, NEW.reverses_entry_year, NEW.reverses_entry_id)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.currency, OLD.line_amount, OLD.line_lc, OLD.line_gc, OLD.reverses_entry_year, OLD.reverses_entry_id) THEN
    RAISE EXCEPTION 'line % of entry % keeps its amounts', OLD.line_no, OLD.entry_id USING ERRCODE = 'check_violation', CONSTRAINT = 'clearing_use_line_fixed';
  END IF;
  RETURN NEW;
END $$;

-- A document's entry records its reversal entry once, with the reversal's
-- document number; nothing else of it changes (the column grant).
ALTER TABLE assets.document_entry
  ADD COLUMN reversal_document_number text,
  ADD CONSTRAINT document_entry_reversal_number CHECK ((reversal_entry_id IS NULL) = (reversal_document_number IS NULL));
GRANT UPDATE (reversal_entry_year, reversal_entry_id, reversal_document_number) ON assets.document_entry TO erp_app;

CREATE FUNCTION assets.check_document_entry() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.reversal_entry_id IS NOT NULL THEN
    RAISE EXCEPTION 'the % entry of % % in ledger % is reversed once', OLD.part, OLD.document_kind, OLD.document_id, OLD.ledger_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'document_entry_reversed_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_document_entry() OWNER TO erp_owner;
CREATE TRIGGER document_entry_reversed_once BEFORE UPDATE ON assets.document_entry FOR EACH ROW EXECUTE FUNCTION assets.check_document_entry();
