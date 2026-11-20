-- The emirate on a sales item (review:batch84; ADR-0067 As built, the
-- emirate on a sales item; ADR-0049 §7, ADR-0063 As built, the emirate on
-- the item): a quotation's or a sales order's item whose output tax code
-- levies the subdivision its lines name (the UAE's sales code S5, one for
-- every emirate) names that subdivision, as a customer invoice's item does
-- (receivables.customer_invoice_item.tax_jurisdiction, 000303), so the
-- order can be billed and the invoice its billing posts carries it to the
-- item's line and tax, where the VAT 201 reads the sale by emirate. Only
-- with a tax code, in the form of an ISO 3166-2 subdivision; that the code
-- levies it is the context's check and, on the posted lines, the ledger's
-- (journal_line_tax_jurisdiction_code). An order item keeps the
-- jurisdiction it was entered with, as it keeps its tax code: the
-- application's column grants (000355) do not include it, and a
-- quotation's items are immutable (000290). A billing document's and a
-- credit memo request's items read it from the order's item. Expand only
-- (06 §6.4): a nullable column, null on every existing item, which the
-- previous version neither reads nor writes, and a check it always
-- passes.
ALTER TABLE sales.quotation_item
  ADD COLUMN tax_jurisdiction text,
  ADD CONSTRAINT quotation_item_tax_jurisdiction
    CHECK (tax_jurisdiction IS NULL OR (tax_code IS NOT NULL AND tax_jurisdiction ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$'));
ALTER TABLE sales.sales_order_item
  ADD COLUMN tax_jurisdiction text,
  ADD CONSTRAINT sales_order_item_tax_jurisdiction
    CHECK (tax_jurisdiction IS NULL OR (tax_code IS NOT NULL AND tax_jurisdiction ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$'));
