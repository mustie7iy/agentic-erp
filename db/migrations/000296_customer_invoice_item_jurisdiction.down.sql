-- A posted invoice keeps its jurisdictions on its journal lines, where the
-- tax return reads them. A draft that names one cannot be posted by the
-- previous version (422 ledger.jurisdiction_required), which refused its
-- tax code at entry: it is entered again after the upgrade.
ALTER TABLE receivables.customer_invoice_item
  DROP CONSTRAINT customer_invoice_item_tax_jurisdiction,
  DROP COLUMN tax_jurisdiction;
