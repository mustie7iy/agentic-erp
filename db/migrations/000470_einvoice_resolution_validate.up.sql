-- Validates the checks 000469 added NOT VALID on einvoicing.einvoice and
-- payables.supplier_invoice (06 §6.4), scanning the tables without
-- blocking writes.
ALTER TABLE payables.supplier_invoice VALIDATE CONSTRAINT supplier_invoice_document_hash;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_status_check;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_entry;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_problem;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_resolved;
ALTER TABLE einvoicing.einvoice VALIDATE CONSTRAINT einvoice_dismissal_reason;
