-- Contract (T61c, ADR-0049 §3; 06-engineering-process §6.4): the former
-- ledger.tax_code, copied into tax_code_definition, tax_component and
-- tax_rate by 000124 and neither read nor written since that release, is
-- dropped; and the index of journal lines by tax code and posting date,
-- which the rate check read before tax items, goes with it: the check reads
-- ledger.tax_item (000141).
DROP TABLE ledger.tax_code;
DROP INDEX ledger.journal_line_tax_code;
