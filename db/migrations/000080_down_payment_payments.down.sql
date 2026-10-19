-- Paid requests and request payments do not survive the downgrade: 000079
-- knows neither.
SET LOCAL row_security = off; -- the down migration changes every tenant's rows (06 §6.4)
ALTER TABLE payables.down_payment_request DISABLE TRIGGER down_payment_request_change;
UPDATE payables.down_payment_request SET status = 'open', journal_entry_year = NULL, journal_entry_id = NULL WHERE status = 'paid';
ALTER TABLE payables.down_payment_request ENABLE TRIGGER down_payment_request_change;
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_paid;
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_status_check;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_status_check CHECK (status IN ('open', 'cancelled'));
ALTER TABLE payables.down_payment_request DROP COLUMN journal_entry_year, DROP COLUMN journal_entry_id;
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
DROP TABLE payments.request_exclusion;
DELETE FROM payments.payment WHERE kind = 'down_payment';
ALTER TABLE payments.payment DROP CONSTRAINT payment_down_payment_request;
ALTER TABLE payments.payment DROP CONSTRAINT payment_kind_check;
ALTER TABLE payments.payment DROP COLUMN kind, DROP COLUMN down_payment_request_id;
