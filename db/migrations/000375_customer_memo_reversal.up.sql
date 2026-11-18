-- A customer credit memo or debit memo reversed (T75k; ADR-0063 As built;
-- 03-data-model §3.26): a posted memo moves to reversed once and names the
-- ledger's reversal of its entry (year and id), the date it posted on and
-- the reason given. Only a credit memo or a debit memo a person entered is
-- reversed: an invoice is corrected by a credit memo (ADR-0063 §5), and a
-- memo sales entered from its own document (source_document_id) is
-- corrected there. The reversal is not dated before the memo posted.
--
-- Expand only (06 §6.4): four nullable columns, a status more, a check on
-- them, the update grant reversing needs and the trigger admitting it; the
-- previous version neither reads nor writes the new columns and never
-- writes the new status. The changed and new checks are added NOT VALID,
-- reading no row under the lock (every row satisfies them: none is
-- reversed), and 000376 validates them.
ALTER TABLE receivables.customer_invoice
  ADD COLUMN reversal_date       date,
  ADD COLUMN reversal_entry_year smallint,
  ADD COLUMN reversal_entry_id   uuid,      -- ledger context
  ADD COLUMN reversal_reason     text,
  DROP CONSTRAINT customer_invoice_status_check,
  ADD CONSTRAINT customer_invoice_status_check CHECK (status IN ('draft', 'posted', 'reversed')) NOT VALID,
  -- A posted or reversed document names its entry, posting date, term and
  -- due date; a draft none of entry and due date.
  DROP CONSTRAINT customer_invoice_posted,
  ADD CONSTRAINT customer_invoice_posted CHECK (
    (status IN ('posted', 'reversed')) = (journal_entry_id IS NOT NULL)
    AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL)
    AND (journal_entry_id IS NULL) = (net_due_date IS NULL)
    AND (status NOT IN ('posted', 'reversed') OR (posting_date IS NOT NULL AND payment_term_code IS NOT NULL))) NOT VALID,
  -- A reversed document names its reversal, the date and a reason of 1 to
  -- 200 characters that is not white space alone, all together; it is a
  -- credit or debit memo a person entered, reversed on or after the date
  -- it posted.
  ADD CONSTRAINT customer_invoice_reversed CHECK (
    (status = 'reversed') = (reversal_entry_id IS NOT NULL)
    AND (reversal_entry_id IS NULL) = (reversal_entry_year IS NULL)
    AND (reversal_entry_id IS NULL) = (reversal_date IS NULL)
    AND (reversal_entry_id IS NULL) = (reversal_reason IS NULL)
    AND (reversal_reason IS NULL OR (length(reversal_reason) BETWEEN 1 AND 200 AND btrim(reversal_reason) <> ''))
    AND (status <> 'reversed' OR (kind IN ('credit_memo', 'debit_memo') AND source_document_id IS NULL AND reversal_date >= posting_date))) NOT VALID;
-- One document per reversal entry.
CREATE UNIQUE INDEX customer_invoice_reversal_once ON receivables.customer_invoice (tenant_id, reversal_entry_id) WHERE reversal_entry_id IS NOT NULL;

-- Posting and reversing are the only changes: a draft moves to posted,
-- once, and what was entered stays (the grant names the columns posting
-- and reversing set); a posted document moves to reversed, once, keeping
-- what it posted with; nothing changes a reversed one.
CREATE OR REPLACE FUNCTION receivables.check_customer_invoice_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'draft' AND NEW.status = 'posted' THEN
    RETURN NEW;
  END IF;
  IF OLD.status = 'posted' AND NEW.status = 'reversed'
      AND (NEW.posting_date, NEW.payment_term_code, NEW.net_due_date, NEW.journal_entry_year, NEW.journal_entry_id)
        IS NOT DISTINCT FROM (OLD.posting_date, OLD.payment_term_code, OLD.net_due_date, OLD.journal_entry_year, OLD.journal_entry_id) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'customer invoice %: only a draft is posted and a posted document reversed, keeping what it posted with', OLD.id
    USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_invoice_posted_once';
END $$;
GRANT UPDATE (reversal_date, reversal_entry_year, reversal_entry_id, reversal_reason) ON receivables.customer_invoice TO erp_app;
