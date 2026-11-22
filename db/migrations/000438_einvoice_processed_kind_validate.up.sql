-- Validates the check 000437 added NOT VALID on einvoicing.einvoice
-- (06 §6.4), scanning the table without blocking writes.
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_read_processed;
