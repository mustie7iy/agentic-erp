-- Restores the kinds before adjustment. Tax items are insert-only, and an
-- adjustment code is kept with the items that name it, so rows of kind
-- adjustment written since stay as they are: the narrower checks come back
-- NOT VALID, holding only the rows written after the down.
ALTER TABLE ledger.tax_item
  DROP CONSTRAINT tax_item_kind_check,
  ADD CONSTRAINT tax_item_kind_check
    CHECK (kind IN ('standard', 'zero', 'exempt', 'out_of_scope', 'reverse_charge', 'import')) NOT VALID;

ALTER TABLE ledger.tax_code_definition
  DROP CONSTRAINT tax_code_definition_kind_check,
  ADD CONSTRAINT tax_code_definition_kind_check
    CHECK (kind IN ('standard', 'zero', 'exempt', 'out_of_scope', 'reverse_charge', 'import')) NOT VALID;
