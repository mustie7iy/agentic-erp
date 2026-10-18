-- Paying down payment requests in payment runs (T46c, ADR-0037,
-- 03-data-model §3.9-3.10). A payment pays either a supplier's open items
-- (kind items, at most one per supplier and run, as before) or one down
-- payment request (kind down_payment), which is never netted with the
-- supplier's items: SAP pays special G/L items separately. Releasing a
-- request's payment posts the down payment and marks the request paid.
ALTER TABLE payments.payment
  ADD COLUMN kind text NOT NULL DEFAULT 'items' CHECK (kind IN ('items', 'down_payment')),
  ADD COLUMN down_payment_request_id uuid;  -- payables context
ALTER TABLE payments.payment ADD CONSTRAINT payment_down_payment_request
  CHECK ((kind = 'down_payment') = (down_payment_request_id IS NOT NULL));
ALTER TABLE payments.payment DROP CONSTRAINT payment_tenant_id_payment_run_id_supplier_id_key;
CREATE UNIQUE INDEX payment_items_once ON payments.payment (tenant_id, payment_run_id, supplier_id) WHERE kind = 'items';
CREATE UNIQUE INDEX payment_request_once ON payments.payment (tenant_id, payment_run_id, down_payment_request_id) WHERE kind = 'down_payment';
-- Finding the live run that holds a request (payments.Reservations.RequestRunOf).
CREATE INDEX payment_by_request ON payments.payment (tenant_id, down_payment_request_id) WHERE kind = 'down_payment';

-- A paid request names the journal entry of its payment, which posted the
-- down payment.
ALTER TABLE payables.down_payment_request
  ADD COLUMN journal_entry_year smallint,
  ADD COLUMN journal_entry_id uuid;          -- ledger context
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_status_check;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_status_check CHECK (status IN ('open', 'paid', 'cancelled'));
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_paid
  CHECK ((status = 'paid') = (journal_entry_id IS NOT NULL) AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL));

-- As 000072's, with open to paid; the payment that paid a request stays its
-- payment.
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
  IF (OLD.status, NEW.status) NOT IN (('open', 'cancelled'), ('open', 'paid')) THEN
    RAISE EXCEPTION 'down payment request % cannot go from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'down_payment_request_lifecycle';
  END IF;
  IF (NEW.document_number, NEW.legal_entity_id, NEW.supplier_id, NEW.purchase_order_id, NEW.currency, NEW.amount, NEW.tax_code, NEW.tax_amount, NEW.due_date,
      NEW.reference, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM
     (OLD.document_number, OLD.legal_entity_id, OLD.supplier_id, OLD.purchase_order_id, OLD.currency, OLD.amount, OLD.tax_code, OLD.tax_amount, OLD.due_date,
      OLD.reference, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'down payment request % is fixed once requested', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'down_payment_request_fixed';
  END IF;
  RETURN NEW;
END $$;
