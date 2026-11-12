-- A billing document posted through receivables (T79b; ADR-0067 §9;
-- ADR-0063 §2; 03-data-model §3.26, §3.28): posting it enters and posts
-- one customer invoice in the same transaction. The billing document
-- moves from created to posted once and names the customer invoice, by
-- id and number, the date it posted on and the journal entry the invoice
-- posted; the customer invoice names the document it came from, by id
-- and number. customer_invoice_id is the receivables context's,
-- journal_entry_id the ledger's, source_document_id the context's that
-- entered the invoice: no foreign key leaves a schema (02 §2.1).
--
-- Expand only (06 §6.4): nullable columns, a status more and the update
-- grant posting needs; the previous version creates billing documents
-- and enters invoices, which stay valid, and neither reads nor writes the
-- new columns.
ALTER TABLE receivables.customer_invoice
  ADD COLUMN source_document_id     uuid,   -- the context that entered the invoice (sales: a billing document)
  ADD COLUMN source_document_number text CHECK (length(source_document_number) BETWEEN 1 AND 40),
  -- An invoice names the document it came from by id and number, or neither.
  ADD CONSTRAINT customer_invoice_source CHECK ((source_document_id IS NULL) = (source_document_number IS NULL));
-- A document is invoiced once.
CREATE UNIQUE INDEX customer_invoice_source_once ON receivables.customer_invoice (tenant_id, source_document_id)
  WHERE source_document_id IS NOT NULL;

ALTER TABLE sales.billing_document
  ADD COLUMN posting_date            date,
  ADD COLUMN customer_invoice_id     uuid,      -- receivables context
  ADD COLUMN customer_invoice_number text CHECK (length(customer_invoice_number) BETWEEN 1 AND 40),
  ADD COLUMN journal_entry_year      smallint,
  ADD COLUMN journal_entry_id        uuid,      -- ledger context
  DROP CONSTRAINT billing_document_status_check,
  ADD CONSTRAINT billing_document_status_check CHECK (status IN ('created', 'posted')),
  -- A posted document names its customer invoice, its posting date and
  -- the invoice's entry; a created one none of them.
  ADD CONSTRAINT billing_document_posted CHECK (
    (status = 'posted') = (customer_invoice_id IS NOT NULL)
    AND (customer_invoice_id IS NULL) = (customer_invoice_number IS NULL)
    AND (customer_invoice_id IS NULL) = (posting_date IS NULL)
    AND (customer_invoice_id IS NULL) = (journal_entry_id IS NULL)
    AND (customer_invoice_id IS NULL) = (journal_entry_year IS NULL));
-- One billing document per customer invoice.
CREATE UNIQUE INDEX billing_document_invoice_once ON sales.billing_document (tenant_id, customer_invoice_id)
  WHERE customer_invoice_id IS NOT NULL;

-- Posting is the only change: a created document moves to posted, once,
-- and what was billed stays (the grant names the columns posting sets).
-- A posted document is not changed again: it is not reversed or
-- cancelled in the first version (ADR-0067 §9).
CREATE FUNCTION sales.check_billing_document_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'created' OR NEW.status <> 'posted' THEN
    RAISE EXCEPTION 'billing document %: only a created document changes, and only by being posted', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'billing_document_posted_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_billing_document_change() OWNER TO erp_owner;
CREATE TRIGGER billing_document_posted_once BEFORE UPDATE ON sales.billing_document
  FOR EACH ROW EXECUTE FUNCTION sales.check_billing_document_change();
GRANT UPDATE (status, posting_date, customer_invoice_id, customer_invoice_number, journal_entry_year, journal_entry_id, version, updated_by)
  ON sales.billing_document TO erp_app;
