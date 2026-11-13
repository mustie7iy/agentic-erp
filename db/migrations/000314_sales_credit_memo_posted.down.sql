DROP INDEX sales.credit_memo_request_posted_by_document;
REVOKE UPDATE (posting_date, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id)
  ON sales.credit_memo_request FROM erp_app;
-- The change rule as 000313 made it.
CREATE OR REPLACE FUNCTION sales.check_credit_memo_request_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'open' OR NEW.status NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'credit memo request %: only an open one changes, and only by being approved or rejected', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_memo_request_decided_once';
  END IF;
  RETURN NEW;
END $$;
-- The status check restored refuses the revert while a posted request
-- exists: the previous version knows no posted one, and its credit memo
-- and entry stay where they are (as 000296's revert does).
ALTER TABLE sales.credit_memo_request
  DROP CONSTRAINT credit_memo_request_invoice_once,
  DROP CONSTRAINT credit_memo_request_posted,
  DROP CONSTRAINT credit_memo_request_status_check,
  ADD CONSTRAINT credit_memo_request_status_check CHECK (status IN ('open', 'approved', 'rejected')),
  DROP COLUMN journal_entry_id,
  DROP COLUMN journal_entry_year,
  DROP COLUMN customer_invoice_number,
  DROP COLUMN customer_invoice_id,
  DROP COLUMN posting_date;
