-- The revert is refused while a document was reversed: the previous
-- version knows drafts and posted documents alone, and the reversal's
-- entry stays in the ledger.
DO $$
BEGIN
  IF EXISTS (SELECT FROM receivables.customer_invoice WHERE status = 'reversed') THEN
    RAISE EXCEPTION 'reversed customer credit or debit memos exist: 000381 cannot be reverted' USING ERRCODE = 'check_violation';
  END IF;
END $$;
REVOKE UPDATE (reversal_date, reversal_entry_year, reversal_entry_id, reversal_reason) ON receivables.customer_invoice FROM erp_app;
-- check_customer_invoice_change as 000271 created it.
CREATE OR REPLACE FUNCTION receivables.check_customer_invoice_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' OR NEW.status <> 'posted' THEN
    RAISE EXCEPTION 'customer invoice %: only a draft changes, and only by being posted', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_invoice_posted_once';
  END IF;
  RETURN NEW;
END $$;
DROP INDEX receivables.customer_invoice_reversal_once;
ALTER TABLE receivables.customer_invoice
  DROP CONSTRAINT customer_invoice_reversed,
  DROP CONSTRAINT customer_invoice_posted,
  ADD CONSTRAINT customer_invoice_posted CHECK (
    (status = 'posted') = (journal_entry_id IS NOT NULL)
    AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL)
    AND (journal_entry_id IS NULL) = (net_due_date IS NULL)
    AND (status <> 'posted' OR (posting_date IS NOT NULL AND payment_term_code IS NOT NULL))),
  DROP CONSTRAINT customer_invoice_status_check,
  ADD CONSTRAINT customer_invoice_status_check CHECK (status IN ('draft', 'posted')),
  DROP COLUMN reversal_reason,
  DROP COLUMN reversal_entry_id,
  DROP COLUMN reversal_entry_year,
  DROP COLUMN reversal_date;
