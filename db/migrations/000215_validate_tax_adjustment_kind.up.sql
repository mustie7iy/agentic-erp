-- Validates the checks 000214 widened NOT VALID (06 §6.4), scanning the
-- tables without blocking writes.
ALTER TABLE ledger.tax_code_definition VALIDATE CONSTRAINT tax_code_definition_kind_check;
ALTER TABLE ledger.tax_item VALIDATE CONSTRAINT tax_item_kind_check;
