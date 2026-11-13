-- A credit memo request posted (T85d; ADR-0067, As built T85d; ADR-0063,
-- As built T85d): an approved request is posted once as a customer credit
-- memo entered and posted in receivables in the same transaction, naming
-- the billing document's invoice. The request then names the date it
-- posted on, the credit memo by id and number and the journal entry that
-- memo posted, all of them once posted and none before, as a posted
-- billing document names its invoice (000296). No new table.
-- Expand only (06 §6.4): nullable columns, a status and a change rule
-- that admits what the previous version never does.
ALTER TABLE sales.credit_memo_request
  ADD COLUMN posting_date date,
  ADD COLUMN customer_invoice_id uuid,         -- receivables context: the customer credit memo
  ADD COLUMN customer_invoice_number text,
  ADD COLUMN journal_entry_year smallint,      -- ledger context: the entry the credit memo posted
  ADD COLUMN journal_entry_id uuid,
  DROP CONSTRAINT credit_memo_request_status_check,
  ADD CONSTRAINT credit_memo_request_status_check CHECK (status IN ('open', 'approved', 'rejected', 'posted')),
  ADD CONSTRAINT credit_memo_request_posted CHECK (
    (status = 'posted') = (posting_date IS NOT NULL) AND (status = 'posted') = (customer_invoice_id IS NOT NULL)
    AND (status = 'posted') = (customer_invoice_number IS NOT NULL) AND (status = 'posted') = (journal_entry_year IS NOT NULL)
    AND (status = 'posted') = (journal_entry_id IS NOT NULL)),
  -- A credit memo is one request's.
  ADD CONSTRAINT credit_memo_request_invoice_once UNIQUE (tenant_id, customer_invoice_id);

-- The decision stays the only change of an open request; an approved
-- request changes once more, by being posted, and nothing else does.
CREATE OR REPLACE FUNCTION sales.check_credit_memo_request_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'approved' AND NEW.status = 'posted' THEN
    RETURN NEW;
  END IF;
  IF OLD.status <> 'open' OR NEW.status NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'credit memo request %: only an open one changes, by being approved or rejected, and an approved one by being posted', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_memo_request_decided_once';
  END IF;
  RETURN NEW;
END $$;
GRANT UPDATE (posting_date, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id)
  ON sales.credit_memo_request TO erp_app;
-- What the posted requests of a billing document credited of its items: the billing item's credited amount.
CREATE INDEX credit_memo_request_posted_by_document ON sales.credit_memo_request (tenant_id, billing_document_id) WHERE status = 'posted';
