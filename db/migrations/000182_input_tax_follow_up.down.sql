ALTER TABLE payments.request_exclusion DROP CONSTRAINT request_exclusion_tax_named,
  DROP COLUMN run_tax_rate, DROP COLUMN request_tax_rate, DROP COLUMN tax_code;
ALTER TABLE payables.supplier_invoice DROP COLUMN documentary_warnings;
