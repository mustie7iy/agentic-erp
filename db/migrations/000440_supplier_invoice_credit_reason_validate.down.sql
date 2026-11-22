-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000439's state by adding the check back NOT VALID.
ALTER TABLE payables.supplier_invoice
  DROP CONSTRAINT supplier_invoice_credit_reason,
  ADD CONSTRAINT supplier_invoice_credit_reason CHECK ((credit_reason_code IS NULL OR kind IN ('credit_memo', 'subsequent_credit'))
    AND (credit_reason_text IS NULL OR credit_reason_code IS NOT NULL)
    AND length(credit_reason_code) BETWEEN 1 AND 100 AND length(credit_reason_text) BETWEEN 1 AND 1000) NOT VALID;
