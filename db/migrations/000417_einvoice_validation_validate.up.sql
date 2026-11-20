-- Validates the checks 000416 added NOT VALID on einvoicing.einvoice
-- (06 §6.4), scanning the table without blocking writes.
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_status_check;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_read_lengths;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_processed;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_problem;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_read;
