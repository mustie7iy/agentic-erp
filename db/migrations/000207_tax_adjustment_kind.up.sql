-- A tax code of kind adjustment (ADR-0049 §11, T61j): tax alone, entered by
-- a person as a manual entry's tax line (an adjustment of net tax, an
-- instalment, a rebate), which a return's box selects by its kind, so it
-- too is a tax item with a document. Its rates are 0 (the configuration
-- and the calculator hold it): its tax is what the entry posts. Expand
-- only: both checks widened NOT VALID (06 §6.4), validated by 000208; the
-- previous release never writes the kind, as its configuration refuses it.

ALTER TABLE ledger.tax_code_definition
  DROP CONSTRAINT tax_code_definition_kind_check,
  ADD CONSTRAINT tax_code_definition_kind_check
    CHECK (kind IN ('standard', 'zero', 'exempt', 'out_of_scope', 'reverse_charge', 'import', 'adjustment')) NOT VALID;

ALTER TABLE ledger.tax_item
  DROP CONSTRAINT tax_item_kind_check,
  ADD CONSTRAINT tax_item_kind_check
    CHECK (kind IN ('standard', 'zero', 'exempt', 'out_of_scope', 'reverse_charge', 'import', 'adjustment')) NOT VALID;
