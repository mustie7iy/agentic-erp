-- Validates the checks 000080 and 000081 added NOT VALID on existing
-- tables (06 §6.4), scanning them without blocking writes.
ALTER TABLE payments.payment VALIDATE CONSTRAINT payment_kind_check;
ALTER TABLE payments.payment VALIDATE CONSTRAINT payment_down_payment_request;
ALTER TABLE payables.down_payment_request VALIDATE CONSTRAINT down_payment_request_status_check;
ALTER TABLE payables.down_payment_request VALIDATE CONSTRAINT down_payment_request_paid;
ALTER TABLE payables.down_payment_request VALIDATE CONSTRAINT down_payment_request_cleared;
ALTER TABLE payables.down_payment_request VALIDATE CONSTRAINT down_payment_request_invoice;
