-- Validates the checks 000233 added NOT VALID on einvoicing.einvoice
-- (06 §6.4), scanning the table without blocking writes.
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_changed_hash;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_changed;
