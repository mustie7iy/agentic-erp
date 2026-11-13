-- A posted invoice keeps its jurisdictions on its journal lines, where the
-- tax return reads them. A draft that names one cannot be posted by the
-- previous version, which refuses its tax code at posting as it did at
-- entry (422 receivables.tax_jurisdiction_not_supported): it is entered
-- again after the upgrade.
ALTER TABLE receivables.customer_invoice_item
  DROP CONSTRAINT customer_invoice_item_tax_jurisdiction,
  DROP COLUMN tax_jurisdiction;
