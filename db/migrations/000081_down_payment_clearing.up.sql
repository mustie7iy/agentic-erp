-- Clearing down payments against the final invoice (T46d, ADR-0037,
-- 03-data-model §3.9). A paid request whose down payment is cleared
-- against a posted invoice of its supplier is cleared, naming the invoice
-- and the transfer entry; the invoice is not reversed while it stands.
ALTER TABLE payables.down_payment_request
  ADD COLUMN cleared_invoice_id uuid,
  ADD COLUMN clearing_entry_year smallint,
  ADD COLUMN clearing_entry_id uuid,          -- ledger context
  ADD CONSTRAINT down_payment_request_invoice FOREIGN KEY (tenant_id, cleared_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id) NOT VALID;
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_status_check;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_status_check CHECK (status IN ('open', 'paid', 'cleared', 'cancelled')) NOT VALID;
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_paid;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_paid
  CHECK ((status IN ('paid', 'cleared')) = (journal_entry_id IS NOT NULL) AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL)) NOT VALID;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_cleared
  CHECK ((status = 'cleared') = (clearing_entry_id IS NOT NULL) AND (clearing_entry_id IS NULL) = (clearing_entry_year IS NULL)
    AND (clearing_entry_id IS NULL) = (cleared_invoice_id IS NULL)) NOT VALID;
-- An invoice's reversal finds the down payments cleared against it.
CREATE INDEX down_payment_request_by_invoice ON payables.down_payment_request (tenant_id, cleared_invoice_id) WHERE cleared_invoice_id IS NOT NULL;

-- As 000080's, with paid to cleared; the payment that paid a request stays
-- its payment.
CREATE OR REPLACE FUNCTION payables.check_down_payment_request_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'open' THEN
      RAISE EXCEPTION 'a down payment request is open first, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'down_payment_request_lifecycle';
    END IF;
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) NOT IN (('open', 'cancelled'), ('open', 'paid'), ('paid', 'cleared')) THEN
    RAISE EXCEPTION 'down payment request % cannot go from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'down_payment_request_lifecycle';
  END IF;
  IF (NEW.document_number, NEW.legal_entity_id, NEW.supplier_id, NEW.purchase_order_id, NEW.currency, NEW.amount, NEW.tax_code, NEW.tax_amount, NEW.due_date,
      NEW.reference, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM
     (OLD.document_number, OLD.legal_entity_id, OLD.supplier_id, OLD.purchase_order_id, OLD.currency, OLD.amount, OLD.tax_code, OLD.tax_amount, OLD.due_date,
      OLD.reference, OLD.created_at, OLD.created_by)
     OR (OLD.journal_entry_id IS NOT NULL AND (NEW.journal_entry_year, NEW.journal_entry_id) IS DISTINCT FROM (OLD.journal_entry_year, OLD.journal_entry_id)) THEN
    RAISE EXCEPTION 'down payment request % is fixed once requested', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'down_payment_request_fixed';
  END IF;
  RETURN NEW;
END $$;
