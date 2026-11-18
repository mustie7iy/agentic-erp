-- Customer debit memos (T75j; ADR-0063 §7 As built; 03-data-model §3.26):
-- the customer invoice document takes a third kind, debit_memo, which
-- charges the customer more and posts as an invoice does, as its own
-- journal document type (DD). Only a credit memo names an invoice
-- (customer_invoice_reference_on_credit, unchanged).
--
-- Expand only (06 §6.4): a kind added to an open enumeration, which the
-- previous version neither writes nor reads. The wider check is added NOT
-- VALID, reading no row under the lock, every row satisfying the narrower
-- one, and 000380 validates it.
ALTER TABLE receivables.customer_invoice
  DROP CONSTRAINT customer_invoice_kind_check,
  ADD CONSTRAINT customer_invoice_kind_check CHECK (kind IN ('invoice', 'credit_memo', 'debit_memo')) NOT VALID;
