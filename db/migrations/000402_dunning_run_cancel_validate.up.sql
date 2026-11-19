-- Validates what 000401 added NOT VALID (06 §6.4).
ALTER TABLE collections.dunning_run VALIDATE CONSTRAINT dunning_run_status_check;
ALTER TABLE collections.dunning_run VALIDATE CONSTRAINT dunning_run_cancelled;
ALTER TABLE collections.dunning_charge VALIDATE CONSTRAINT dunning_charge_reversed;
ALTER TABLE receivables.customer_invoice VALIDATE CONSTRAINT customer_invoice_reversed;
