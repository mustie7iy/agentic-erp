-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000090's state by adding it back NOT VALID.
ALTER TABLE partner.supplier_company DROP CONSTRAINT supplier_company_payment_methods;
ALTER TABLE partner.supplier_company ADD CONSTRAINT supplier_company_payment_methods
  CHECK (cardinality(payment_methods) = 0 OR array_to_string(payment_methods, ',', '?') ~ '^[A-Z0-9](,[A-Z0-9]){0,9}$') NOT VALID;
