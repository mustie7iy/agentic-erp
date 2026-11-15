-- A validated check cannot be marked unvalidated again: the down restores
-- 000334's state by adding it back NOT VALID.
ALTER TABLE payables.upload DROP CONSTRAINT upload_purpose_check;
ALTER TABLE payables.upload ADD CONSTRAINT upload_purpose_check CHECK (purpose IN ('supplier_invoice', 'sales_order')) NOT VALID;
