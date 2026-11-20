-- A posted billing document keeps its items' jurisdictions on its
-- customer invoice and journal lines, where the tax return reads them. An
-- order or quotation item that names one loses it: the previous version
-- refuses such a code at entry (422 sales.tax_jurisdiction_not_supported)
-- and bills such an order's items only for the posting to refuse them (422
-- receivables.tax_jurisdiction_required), so nothing posts without its
-- emirate; the order is entered again after the upgrade.
ALTER TABLE sales.sales_order_item
  DROP CONSTRAINT sales_order_item_tax_jurisdiction,
  DROP COLUMN tax_jurisdiction;
ALTER TABLE sales.quotation_item
  DROP CONSTRAINT quotation_item_tax_jurisdiction,
  DROP COLUMN tax_jurisdiction;
