-- Down payment requests (T46b, ADR-0037, 03-data-model §3.9). A request
-- (SAP F-47, special G/L F, a noted item) asks for a down payment to a
-- supplier. It is a payables document, not a ledger posting: it moves no
-- value until it is paid (T46c), which posts the special G/L item A.
CREATE TABLE payables.down_payment_request (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  document_number    text NOT NULL,                -- from the down_payment_request number range
  legal_entity_id    uuid NOT NULL,                -- tenancy context
  supplier_id        uuid NOT NULL,                -- partner context
  purchase_order_id  uuid,                         -- procurement context
  currency           char(3) NOT NULL REFERENCES ref.currency,
  amount             bigint NOT NULL CHECK (amount > 0),  -- what is paid, tax included
  tax_code           text,                         -- an input tax code; the payment claims the tax (T46c)
  tax_amount         bigint NOT NULL DEFAULT 0,
  due_date           date NOT NULL,
  reference          text CHECK (reference IS NULL OR length(trim(reference)) > 0),
  status             text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'cancelled')),
  cancel_reason      text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid NOT NULL,
  version            integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT down_payment_request_number UNIQUE (tenant_id, legal_entity_id, document_number),
  CONSTRAINT down_payment_request_tax CHECK (CASE WHEN tax_code IS NULL THEN tax_amount = 0 ELSE tax_amount >= 0 AND tax_amount < amount END),
  CONSTRAINT down_payment_request_cancel_reason CHECK ((status = 'cancelled') = (cancel_reason IS NOT NULL))
);
-- Payment runs pick open requests by legal entity and due date (T46c).
CREATE INDEX down_payment_request_by_status ON payables.down_payment_request (tenant_id, legal_entity_id, status, due_date);
CREATE INDEX down_payment_request_by_supplier ON payables.down_payment_request (tenant_id, supplier_id);
CREATE TRIGGER down_payment_request_touch BEFORE UPDATE ON payables.down_payment_request FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- A request is open first and moves only along its lifecycle; what was
-- requested, and by whom, is never rewritten. Callers read the request FOR
-- UPDATE before they change it (ADR-0008).
CREATE FUNCTION payables.check_down_payment_request_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'open' THEN
      RAISE EXCEPTION 'a down payment request is open first, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'down_payment_request_lifecycle';
    END IF;
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) NOT IN (('open', 'cancelled')) THEN
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
ALTER FUNCTION payables.check_down_payment_request_change() OWNER TO erp_owner;
CREATE TRIGGER down_payment_request_change BEFORE INSERT OR UPDATE ON payables.down_payment_request
  FOR EACH ROW EXECUTE FUNCTION payables.check_down_payment_request_change();

SELECT platform.secure_table('payables.down_payment_request'::regclass);

-- Requesting a down payment pays a supplier before any invoice backs it,
-- so it is its own permission (SAP F-47's authorization), apart from
-- capturing invoices.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('payables.down_payment.request', 'Request and cancel down payments to suppliers', false);
