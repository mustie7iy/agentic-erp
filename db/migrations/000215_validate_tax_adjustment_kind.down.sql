-- A validated constraint cannot be marked unvalidated again; the down
-- restores 000214's state by adding the checks back NOT VALID.
ALTER TABLE ledger.tax_item
  DROP CONSTRAINT tax_item_kind_check,
  ADD CONSTRAINT tax_item_kind_check
    CHECK (kind IN ('standard', 'zero', 'exempt', 'out_of_scope', 'reverse_charge', 'import', 'adjustment')) NOT VALID;

ALTER TABLE ledger.tax_code_definition
  DROP CONSTRAINT tax_code_definition_kind_check,
  ADD CONSTRAINT tax_code_definition_kind_check
    CHECK (kind IN ('standard', 'zero', 'exempt', 'out_of_scope', 'reverse_charge', 'import', 'adjustment')) NOT VALID;
