-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000247's state by adding it back NOT VALID.
ALTER TABLE payments.payment_method DROP CONSTRAINT payment_method_run_order;
ALTER TABLE payments.payment_method ADD CONSTRAINT payment_method_run_order CHECK (run_order > 0) NOT VALID;
