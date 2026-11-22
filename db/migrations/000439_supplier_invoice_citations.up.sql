-- E-invoice credit notes as one credit memo (ADR-0061 §6, As built batch
-- 88; po:2026-11-21): a received tax credit note keeps its reason (BTAE-03,
-- code and words) and every preceding invoice it cites (IBG-03: the number
-- IBT-025, the issue date IBT-026 when stated, and the posted invoice of
-- the supplier it was matched to, if any). A credit note citing several
-- invoices is one credit memo citing them all, its reference_invoice_id
-- none, since no one invoice is what it corrects; one citing one names it
-- there as before; a volume discount (VD) may cite none (IBR-055-AE).
-- The reason belongs to what credits only, its words not without its code.
ALTER TABLE payables.supplier_invoice
  ADD COLUMN credit_reason_code text,
  ADD COLUMN credit_reason_text text,
  ADD CONSTRAINT supplier_invoice_credit_reason CHECK ((credit_reason_code IS NULL OR kind IN ('credit_memo', 'subsequent_credit'))
    AND (credit_reason_text IS NULL OR credit_reason_code IS NOT NULL)
    AND length(credit_reason_code) BETWEEN 1 AND 100 AND length(credit_reason_text) BETWEEN 1 AND 1000) NOT VALID;

-- The preceding invoices a credit memo cites, in the document's order, each
-- number once; written with the memo and never changed: what the document
-- states. cited_invoice_id is the supplier's posted invoice the number (and
-- date) matched when the memo was entered; none when none did, which holds
-- the memo for a person.
CREATE TABLE payables.supplier_invoice_citation (
  tenant_id            uuid NOT NULL,
  supplier_invoice_id  uuid NOT NULL,
  position             smallint NOT NULL CHECK (position BETWEEN 1 AND 100),
  invoice_number       text NOT NULL CHECK (btrim(invoice_number) <> '' AND length(invoice_number) <= 200),
  invoice_date         date,
  cited_invoice_id     uuid,
  created_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, supplier_invoice_id, position),
  CONSTRAINT supplier_invoice_citation_once UNIQUE (tenant_id, supplier_invoice_id, invoice_number),
  CONSTRAINT supplier_invoice_citation_invoice_fkey FOREIGN KEY (tenant_id, supplier_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id),
  CONSTRAINT supplier_invoice_citation_cited_fkey FOREIGN KEY (tenant_id, cited_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id)
);
CREATE INDEX supplier_invoice_citation_cited ON payables.supplier_invoice_citation (tenant_id, cited_invoice_id) WHERE cited_invoice_id IS NOT NULL;
CREATE TRIGGER supplier_invoice_citation_immutable BEFORE UPDATE OR DELETE ON payables.supplier_invoice_citation
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('payables.supplier_invoice_citation'::regclass);
