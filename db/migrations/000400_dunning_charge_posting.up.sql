-- Dunning charges and interest posted to the customer (T76h; ADR-0065 As
-- built; 03-data-model §3.27): a confirmed run posts each notice's charge
-- and interest as one customer debit memo through receivables, whose
-- journal entry debits the customer and credits the company's dunning
-- charge and interest accounts (account determination dunning_charges,
-- dunning_interest). The notice names its memo here: the memo's id and
-- number (receivables context), its journal entry (ledger context) and
-- the date it posted on. The memo names the notice as its source
-- (receivables.customer_invoice.source_document_id), once per notice
-- (customer_invoice_source_once); no foreign key leaves the schema (02
-- §2.1).
--
-- Expand only (06 §6.4): a new table, which the previous version neither
-- reads nor writes.
CREATE TABLE collections.dunning_charge (
  tenant_id                uuid NOT NULL,
  dunning_notice_id        uuid NOT NULL,
  dunning_run_id           uuid NOT NULL,
  customer_invoice_id      uuid NOT NULL,     -- receivables context: the debit memo
  customer_invoice_number  text NOT NULL CONSTRAINT dunning_charge_invoice_number_check CHECK (length(customer_invoice_number) BETWEEN 1 AND 40),
  journal_entry_year       smallint NOT NULL, -- ledger context: the memo's entry
  journal_entry_id         uuid NOT NULL,
  posting_date             date NOT NULL,
  -- A notice is charged once.
  PRIMARY KEY (tenant_id, dunning_notice_id),
  CONSTRAINT dunning_charge_notice_fkey FOREIGN KEY (tenant_id, dunning_run_id, dunning_notice_id)
    REFERENCES collections.dunning_notice (tenant_id, dunning_run_id, id),
  -- A memo charges one notice.
  CONSTRAINT dunning_charge_invoice_once UNIQUE (tenant_id, customer_invoice_id)
);
-- A run's charges, which its count and a cancellation (T76i) read.
CREATE INDEX dunning_charge_by_run ON collections.dunning_charge (tenant_id, dunning_run_id);
-- What was posted is fixed.
CREATE TRIGGER dunning_charge_immutable BEFORE UPDATE OR DELETE ON collections.dunning_charge
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('collections.dunning_charge'::regclass);
