-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000081's state by adding each one back NOT VALID.
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_invoice;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_invoice
  FOREIGN KEY (tenant_id, cleared_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id) NOT VALID;
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_cleared;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_cleared
  CHECK ((status = 'cleared') = (clearing_entry_id IS NOT NULL) AND (clearing_entry_id IS NULL) = (clearing_entry_year IS NULL)
    AND (clearing_entry_id IS NULL) = (cleared_invoice_id IS NULL)) NOT VALID;
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_paid;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_paid
  CHECK ((status IN ('paid', 'cleared')) = (journal_entry_id IS NOT NULL) AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL)) NOT VALID;
ALTER TABLE payables.down_payment_request DROP CONSTRAINT down_payment_request_status_check;
ALTER TABLE payables.down_payment_request ADD CONSTRAINT down_payment_request_status_check
  CHECK (status IN ('open', 'paid', 'cleared', 'cancelled')) NOT VALID;
ALTER TABLE payments.payment DROP CONSTRAINT payment_down_payment_request;
ALTER TABLE payments.payment ADD CONSTRAINT payment_down_payment_request
  CHECK ((kind = 'down_payment') = (down_payment_request_id IS NOT NULL)) NOT VALID;
ALTER TABLE payments.payment DROP CONSTRAINT payment_kind_check;
ALTER TABLE payments.payment ADD CONSTRAINT payment_kind_check CHECK (kind IN ('items', 'down_payment')) NOT VALID;
