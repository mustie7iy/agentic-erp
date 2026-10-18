-- Paying down payment requests in payment runs (T46c, ADR-0037,
-- 03-data-model §3.9-3.10). A payment pays either a supplier's open items
-- (kind items, at most one per supplier and run, as before) or one down
-- payment request (kind down_payment), which is never netted with the
-- supplier's items: SAP pays special G/L items separately. Releasing a
-- request's payment posts the down payment and marks the request paid.
-- Checks on existing tables are added NOT VALID and validated in 000075
-- (06 §6.4); new rows are checked from here on.
ALTER TABLE payments.payment
  ADD COLUMN kind text NOT NULL DEFAULT 'items',
  ADD COLUMN down_payment_request_id uuid;  -- payables context
ALTER TABLE payments.payment ADD CONSTRAINT payment_kind_check CHECK (kind IN ('items', 'down_payment')) NOT VALID;
ALTER TABLE payments.payment ADD CONSTRAINT payment_down_payment_request
  CHECK ((kind = 'down_payment') = (down_payment_request_id IS NOT NULL)) NOT VALID;
-- One items payment per supplier and run, one per request: the unique
-- indexes replacing UNIQUE (tenant_id, payment_run_id, supplier_id) are
-- built concurrently (000076-000078) and the constraint dropped after them
-- (000079), each in a migration of its own (06 §6.4).

-- A request a proposal leaves out has an exclusion of its own: its supplier
-- cannot be paid, or its tax code is not valid on the run date
-- (tax_code_invalid). payment_exclusion keeps the suppliers' items, one row
-- per run and supplier, as before.
CREATE TABLE payments.request_exclusion (
  tenant_id                uuid NOT NULL,
  payment_run_id           uuid NOT NULL,
  down_payment_request_id  uuid NOT NULL,  -- payables context
  supplier_id              uuid NOT NULL,  -- partner context
  reason_code              text NOT NULL CHECK (reason_code IN ('supplier_blocked', 'payment_blocked', 'no_bank_account', 'bank_change_pending',
                                                                'bank_change_hold', 'tax_code_invalid')),
  amount                   bigint NOT NULL CHECK (amount > 0),
  PRIMARY KEY (tenant_id, payment_run_id, down_payment_request_id),
  FOREIGN KEY (tenant_id, payment_run_id) REFERENCES payments.payment_run (tenant_id, id)
);
SELECT platform.secure_table('payments.request_exclusion'::regclass);

-- A paid request names the journal entry of its payment, which posted the
-- down payment.
ALTER TABLE payables.down_payment_request
  ADD COLUMN journal_entry_year smallint,
  ADD COLUMN journal_entry_id uuid;          -- ledger context
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_status_check;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_status_check CHECK (status IN ('open', 'paid', 'cancelled')) NOT VALID;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_paid
  CHECK ((status = 'paid') = (journal_entry_id IS NOT NULL) AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL)) NOT VALID;

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
