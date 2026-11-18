-- The kind check restored refuses the revert while a debit memo exists:
-- the previous version knows invoices and credit memos alone, and a posted
-- debit memo's entry stays in the ledger.
ALTER TABLE receivables.customer_invoice
  DROP CONSTRAINT customer_invoice_kind_check,
  ADD CONSTRAINT customer_invoice_kind_check CHECK (kind IN ('invoice', 'credit_memo'));
