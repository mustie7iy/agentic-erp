-- Customer credit memos (T75d; ADR-0063 §7; 03-data-model §3.26): the
-- customer invoice document takes a second kind, credit_memo, posted with
-- the signs turned, and may name the invoice it credits
-- (reference_invoice_id): a document of the same tenant, and only a credit
-- memo names one. That the invoice named is a posted invoice of the same
-- company, customer and currency is checked when the memo is entered, and
-- that the posted credits naming it stay within its gross amount when the
-- memo posts, under the invoice's row lock (receivables.PostInvoice): both
-- read other rows, which a check constraint cannot.
--
-- Expand only (06 §6.4): a nullable column and a kind more; the previous
-- version enters invoices, which stay valid, and neither reads nor writes
-- the new column.
ALTER TABLE receivables.customer_invoice
  ADD COLUMN reference_invoice_id uuid,
  DROP CONSTRAINT customer_invoice_kind_check,
  ADD CONSTRAINT customer_invoice_kind_check CHECK (kind IN ('invoice', 'credit_memo')),
  ADD CONSTRAINT customer_invoice_reference_fkey FOREIGN KEY (tenant_id, reference_invoice_id)
    REFERENCES receivables.customer_invoice (tenant_id, id),
  ADD CONSTRAINT customer_invoice_reference_on_credit CHECK (reference_invoice_id IS NULL OR kind = 'credit_memo'),
  ADD CONSTRAINT customer_invoice_reference_not_itself CHECK (reference_invoice_id <> id);
-- The credits naming an invoice, summed when another posts.
CREATE INDEX customer_invoice_by_reference ON receivables.customer_invoice (tenant_id, reference_invoice_id)
  WHERE reference_invoice_id IS NOT NULL;
