-- A supplier invoice's tax date (T61d, ADR-0049 §4, decision P6): the date
-- its tax codes' rates apply on and its tax is reported in. None: the
-- invoice date, or, for a correction naming an invoice, that invoice's tax
-- date. A person sets it, never an agent (the application refuses one).
-- Expand only: a nullable column.
ALTER TABLE payables.supplier_invoice ADD COLUMN tax_date date;
