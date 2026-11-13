-- The emirate on a customer invoice's item (product owner 2026-11-12;
-- ADR-0063 §4 As built, ADR-0049 §7): an item whose output tax code levies
-- the subdivision its lines name (the UAE's sales code, one for every
-- emirate) names that subdivision, as a journal line does
-- (ledger.journal_line.tax_jurisdiction, 000150), and the invoice's
-- posting carries it to the item's line and its tax, so the VAT 201 reads
-- the sale in its emirate. Only with a tax code, in the form of an ISO
-- 3166-2 subdivision; that the code levies it is the context's check and,
-- on the posted lines, the ledger's (journal_line_tax_jurisdiction_code).
-- Expand only: a nullable column, null on every existing item, which the
-- previous version neither reads nor writes (06 §6.4).
ALTER TABLE receivables.customer_invoice_item ADD COLUMN tax_jurisdiction text;
ALTER TABLE receivables.customer_invoice_item ADD CONSTRAINT customer_invoice_item_tax_jurisdiction
  CHECK (tax_jurisdiction IS NULL OR (tax_code IS NOT NULL AND tax_jurisdiction ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$'));
