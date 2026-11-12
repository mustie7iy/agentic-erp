-- A customer invoice posted (T75b; ADR-0063 §5; 03-data-model §3.26): the
-- invoice moves from draft to posted once and names the journal entry
-- that is the customer's open item, the posting date it posted on, the
-- payment term it posted with and its net due date. The term is the one
-- entered with the invoice (payment_term_code, new and optional), else the
-- customer's in the company (partner.customer_company); the ledger keeps
-- the terms with the open item (ledger.open_item_terms), so net_due_date
-- here is what the item's terms give and never moves.
--
-- Expand only (06 §6.4): four nullable columns, a status more, and the
-- update grant posting needs; the previous version enters drafts, which
-- stay valid, and neither reads nor writes the new columns.
ALTER TABLE receivables.customer_invoice
  ADD COLUMN payment_term_code  text CHECK (length(payment_term_code) BETWEEN 1 AND 20),  -- partner context
  ADD COLUMN net_due_date       date,
  ADD COLUMN journal_entry_year smallint,
  ADD COLUMN journal_entry_id   uuid,      -- ledger context
  DROP CONSTRAINT customer_invoice_status_check,
  ADD CONSTRAINT customer_invoice_status_check CHECK (status IN ('draft', 'posted')),
  -- A posted invoice names its entry, posting date, term and due date; a
  -- draft none of entry and due date.
  ADD CONSTRAINT customer_invoice_posted CHECK (
    (status = 'posted') = (journal_entry_id IS NOT NULL)
    AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL)
    AND (journal_entry_id IS NULL) = (net_due_date IS NULL)
    AND (status <> 'posted' OR (posting_date IS NOT NULL AND payment_term_code IS NOT NULL)));
-- One invoice per journal entry.
CREATE UNIQUE INDEX customer_invoice_entry_once ON receivables.customer_invoice (tenant_id, journal_entry_id) WHERE journal_entry_id IS NOT NULL;

-- Posting is the only change: a draft moves to posted, once, and what was
-- entered stays (the grant names the columns posting sets). A posted
-- invoice is not changed again: it is not reversed in the first version
-- and is corrected by a credit memo (ADR-0063 §5).
CREATE FUNCTION receivables.check_customer_invoice_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' OR NEW.status <> 'posted' THEN
    RAISE EXCEPTION 'customer invoice %: only a draft changes, and only by being posted', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'customer_invoice_posted_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION receivables.check_customer_invoice_change() OWNER TO erp_owner;
CREATE TRIGGER customer_invoice_posted_once BEFORE UPDATE ON receivables.customer_invoice
  FOR EACH ROW EXECUTE FUNCTION receivables.check_customer_invoice_change();
GRANT UPDATE (status, posting_date, payment_term_code, net_due_date, journal_entry_year, journal_entry_id, version, updated_by)
  ON receivables.customer_invoice TO erp_app;
