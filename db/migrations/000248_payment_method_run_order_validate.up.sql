-- Validates the check 000247 added NOT VALID (06 §6.4): every order it
-- wrote is a place in a list, from 1.
ALTER TABLE payments.payment_method VALIDATE CONSTRAINT payment_method_run_order;
