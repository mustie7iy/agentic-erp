-- Resolving an accepted dispute case (T86d; ADR-0065 As built; 03-data-model
-- §3.27): an open case the company accepts, in full or in part, is closed as
-- accepted with its resolution, a customer credit memo per accepted item
-- (receivables) or a write-off of the accepted amounts (the ledger, source
-- context collections), the amount accepted of each item and in all, the
-- date the documents posted on and the documents themselves. The credit
-- memos and the write-off entry are other contexts' rows, named by id and
-- number: no foreign key leaves the schema (02 §2.1).
--
-- Expand only (06 §6.4): nullable columns, a wider check on the journal's
-- source contexts (NOT VALID, which reads no row under the lock; 000405
-- validates it), a wider grant and a permission, which the previous
-- version neither reads nor writes. The functions replaced keep what they
-- held for a case without a resolution, every case the previous version
-- writes.

-- A case's resolution: what resolved it, what it accepted in its currency
-- and, as its items were booked, in the company's, the date its documents
-- posted on, and a write-off's journal entry. Only an accepted case has a
-- resolution, which accepts more than zero and at most what it disputes; a
-- write-off names its entry, a credit memo resolution none (its items name
-- their memos).
ALTER TABLE collections.dispute_case
  ADD COLUMN resolution             text CONSTRAINT dispute_case_resolution_check CHECK (resolution IN ('credit_memo', 'write_off')),
  ADD COLUMN accepted_amount        bigint,
  ADD COLUMN accepted_amount_lc     bigint CONSTRAINT dispute_case_accepted_amount_lc_check CHECK (accepted_amount_lc >= 0),
  ADD COLUMN posting_date           date,
  ADD COLUMN write_off_entry_year   smallint,
  ADD COLUMN write_off_entry_id     uuid,       -- ledger context
  ADD COLUMN write_off_entry_number text CONSTRAINT dispute_case_write_off_entry_number_check CHECK (length(write_off_entry_number) BETWEEN 1 AND 40),
  ADD CONSTRAINT dispute_case_resolved_check CHECK ((resolution IS NULL OR status = 'accepted')
    AND (resolution IS NULL) = (accepted_amount IS NULL) AND (resolution IS NULL) = (accepted_amount_lc IS NULL)
    AND (resolution IS NULL) = (posting_date IS NULL)
    AND (accepted_amount IS NULL OR accepted_amount BETWEEN 1 AND disputed_amount)),
  ADD CONSTRAINT dispute_case_write_off_check CHECK ((resolution IS NOT DISTINCT FROM 'write_off') = (write_off_entry_id IS NOT NULL)
    AND (write_off_entry_id IS NULL) = (write_off_entry_year IS NULL) AND (write_off_entry_id IS NULL) = (write_off_entry_number IS NULL));
-- The resolution is written with the close, in one update of the open case.
GRANT UPDATE (resolution, accepted_amount, accepted_amount_lc, posting_date, write_off_entry_year, write_off_entry_id, write_off_entry_number)
  ON collections.dispute_case TO erp_app;

-- An item's part of the resolution: what was accepted of it, from zero to
-- what it disputes, in the case's currency and in the company's as booked,
-- and the credit memo that credited it, for an item accepted in part or in
-- full by a credit memo resolution.
ALTER TABLE collections.dispute_case_item
  ADD COLUMN accepted_amount     bigint,
  ADD COLUMN accepted_amount_lc  bigint CONSTRAINT dispute_case_item_accepted_amount_lc_check CHECK (accepted_amount_lc >= 0),
  ADD COLUMN credit_memo_id      uuid,       -- receivables context
  ADD COLUMN credit_memo_number  text CONSTRAINT dispute_case_item_credit_memo_number_check CHECK (length(credit_memo_number) BETWEEN 1 AND 40),
  ADD CONSTRAINT dispute_case_item_accepted_check CHECK ((accepted_amount IS NULL) = (accepted_amount_lc IS NULL)
    AND (accepted_amount IS NULL OR accepted_amount BETWEEN 0 AND disputed_amount)
    AND (credit_memo_id IS NULL) = (credit_memo_number IS NULL) AND (credit_memo_id IS NULL OR accepted_amount > 0));
GRANT UPDATE (accepted_amount, accepted_amount_lc, credit_memo_id, credit_memo_number) ON collections.dispute_case_item TO erp_app;

-- An item stays as its case named it; its part of the resolution is written
-- once, while the case is open (the case is closed with it, in the same
-- transaction); only its case's close marks it no longer open.
CREATE OR REPLACE FUNCTION collections.check_dispute_case_item_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  resolved constant text[] := ARRAY['open', 'accepted_amount', 'accepted_amount_lc', 'credit_memo_id', 'credit_memo_number'];
BEGIN
  IF TG_OP = 'DELETE' OR (to_jsonb(NEW) - resolved) <> (to_jsonb(OLD) - resolved) OR (NEW.open AND NOT OLD.open)
      OR (OLD.accepted_amount IS NOT NULL AND (NEW.accepted_amount, NEW.accepted_amount_lc, NEW.credit_memo_id, NEW.credit_memo_number)
        IS DISTINCT FROM (OLD.accepted_amount, OLD.accepted_amount_lc, OLD.credit_memo_id, OLD.credit_memo_number))
      OR (OLD.accepted_amount IS NULL AND NEW.accepted_amount IS NOT NULL AND NOT OLD.open) THEN
    RAISE EXCEPTION 'dispute case item %/%: an item stays as its case named it, its resolution is written once while its case is open, '
      'and it is closed with its case', OLD.dispute_case_id, OLD.item_no USING ERRCODE = 'check_violation', CONSTRAINT = 'dispute_case_item_fixed';
  END IF;
  RETURN NEW;
END $$;

-- At commit, beside what 000403 holds: a resolved case's items each have
-- what was accepted of them, together its accepted amount in both
-- currencies, a credit memo exactly on each item accepted above zero of a
-- credit memo resolution; a case without a resolution has no item with
-- one.
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
  accepted integer;
  accepted_total numeric;
  accepted_lc numeric;
  memos_wrong integer;
BEGIN
  IF TG_TABLE_NAME = 'dispute_case' THEN
    case_id := NEW.id;
  ELSE
    case_id := NEW.dispute_case_id;
  END IF;
  SELECT * INTO c FROM collections.dispute_case WHERE tenant_id = NEW.tenant_id AND id = case_id;
  SELECT count(*), sum(i.disputed_amount), sum(i.disputed_amount_lc),
         bool_or(i.item_no = 1 AND i.fiscal_year = c.fiscal_year AND i.journal_entry_id = c.journal_entry_id AND i.line_no = c.line_no),
         count(*) FILTER (WHERE i.open <> (c.status = 'open')),
         count(i.accepted_amount), sum(i.accepted_amount), sum(i.accepted_amount_lc),
         count(*) FILTER (WHERE (i.credit_memo_id IS NOT NULL) <> (c.resolution IS NOT DISTINCT FROM 'credit_memo' AND i.accepted_amount > 0))
    INTO n, total, total_lc, first_line, mismatched, accepted, accepted_total, accepted_lc, memos_wrong
    FROM collections.dispute_case_item i WHERE i.tenant_id = c.tenant_id AND i.dispute_case_id = c.id;
  IF n = 0 OR total <> c.disputed_amount OR total_lc <> c.disputed_amount_lc OR NOT first_line OR mismatched > 0 THEN
    RAISE EXCEPTION 'dispute case %: its items are its amounts, its first is its own line, and they are open while it is', c.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dispute_case_items';
  END IF;
  IF (c.resolution IS NULL AND accepted > 0) OR (c.resolution IS NOT NULL AND (accepted <> n OR accepted_total <> c.accepted_amount
      OR accepted_lc <> c.accepted_amount_lc)) OR memos_wrong > 0 THEN
    RAISE EXCEPTION 'dispute case %: a resolved case''s items are what it accepted, each credit memo of an item accepted by one', c.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dispute_case_items_resolved';
  END IF;
  RETURN NULL;
END $$;

-- A write-off of a dispute case is posted by the collections context.
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling', 'revenue',
                            'leases', 'inventory', 'collections')) NOT VALID;

-- Resolving dispute cases, which posts credit memos and write-offs (ADR-0065
-- As built, T86d).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('collections.dispute.resolve', 'Resolve accepted dispute cases by a customer credit memo or a write-off, which posts them', false);
